package admin

// This file implements ADM-1, the admin HTTP server lifecycle. SEC-6 depends on it because
// the console must be bindable, startable and drainable from the daemon.
//
// The lifecycle deliberately mirrors internal/metrics/server.go and cmd/lattice/pprof.go:
// bind SYNCHRONOUSLY in the constructor so a port conflict fails fast at initialization
// rather than at Start(), then run the accept loop in a background goroutine, then drain
// within a caller-supplied deadline.

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/silent-knight19/lattice/internal/transport"
)

// Metric names registered by the daemon when it wires the console.
const (
	// AdminEventsDroppedMetric counts events dropped because a subscriber fell behind.
	AdminEventsDroppedMetric = "lattice_admin_events_dropped"
	// AdminSubscribersMetric reports currently connected event-stream subscribers.
	AdminSubscribersMetric = "lattice_admin_subscribers"
)

// DefaultAdminPort is the documented console port.
const DefaultAdminPort = 7070

// Server is the Lattice Console admin HTTP server.
type Server struct {
	listener net.Listener
	http     *http.Server
	addr     string
	loopback bool

	policy *transport.AuthzPolicy
	bus    *EventBus
	static *StaticHandler
	router *Router

	// Derived at construction from the address the kernel actually bound.
	hosts         *HostAllowlist
	origins       *OriginAllowlist
	servingScheme string
	csrf          *CSRFToken

	started atomic.Bool
	closed  atomic.Bool
	serveEr error
	done    chan struct{}

	mu       sync.Mutex
	resolver PrincipalResolver
}

// ServerOptions configures a Server.
type ServerOptions struct {
	// Bind controls the listener. Addr empty selects the default loopback port.
	Bind BindOptions
	// Policy maps certificate fingerprints to roles. Nil means no client certificate
	// is trusted, which fails closed for every request.
	Policy *transport.AuthzPolicy
	// Handlers supplies the endpoint implementations. Nil fields are simply not
	// registered, so the API can grow incrementally without ever exposing a stub.
	Handlers *Handlers
	// Static, when nil, is built from the embedded placeholder.
	Static *StaticHandler
}

// NewServer binds the listener and assembles the full middleware chain and router.
//
// It returns an error for any bind failure. A nil *Server with a nil error is never
// returned; callers that want a disabled console should not call this at all.
func NewServer(opts ServerOptions) (*Server, error) {
	bind := opts.Bind
	if bind.Addr == "" {
		bind.Addr = fmt.Sprintf("127.0.0.1:%d", DefaultAdminPort)
	}
	if bind.Scheme == "" {
		bind.Scheme = "http"
	}

	res, err := BindLoopback(bind)
	if err != nil {
		return nil, err
	}

	static := opts.Static
	if static == nil {
		if static, err = NewStaticHandler(); err != nil {
			_ = res.Listener.Close()
			return nil, fmt.Errorf("admin server error: loading embedded console assets: %w", err)
		}
	}

	bus := NewEventBus(BusOptions{MaxSubscribers: DefaultMaxSubscribers})
	csrf, err := NewCSRFToken()
	if err != nil {
		_ = res.Listener.Close()
		return nil, fmt.Errorf("admin server error: generating CSRF token: %w", err)
	}

	// Fill in the endpoints that exist in this package today. The rest arrive with their
	// phases; an absent handler means the route is simply absent (default-deny).
	h := opts.Handlers
	if h == nil {
		h = &Handlers{}
	}
	if h.Session == nil {
		h.Session = SessionHandler(csrf)
	}
	if h.Events == nil {
		h.Events = newSSEStreamHandler(bus)
	}

	specs := BuildRoutes(h)
	router, err := NewRouter(specs, static)
	if err != nil {
		_ = res.Listener.Close()
		return nil, fmt.Errorf("admin server error: %w", err)
	}

	s := &Server{
		listener:      res.Listener,
		addr:          res.Addr,
		loopback:      res.Loopback,
		policy:        opts.Policy,
		bus:           bus,
		static:        static,
		router:        router,
		hosts:         res.Hosts,
		origins:       res.Origins,
		servingScheme: bind.Scheme,
		csrf:          csrf,
		done:          make(chan struct{}),
	}
	s.resolver = s.defaultResolver

	s.http = NewHTTPServer(s.handler())
	return s, nil
}

// defaultResolver derives the principal from the verified client certificate, if any.
//
// With no TLS configured (the loopback default) there is no peer certificate, so the
// resolver returns nil and every request is DENIED. That is deliberate: an unauthenticated
// console must not silently grant admin rights to whoever can reach the socket.
//
// Callers that front the console with a trusted proxy, or that enable client TLS, install
// their own resolver with SetPrincipalResolver.
func (s *Server) defaultResolver(r *http.Request) *Principal {
	if s.policy == nil || s.policy.IsEmpty() {
		return nil
	}
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		return nil
	}
	fp := transport.CertificateFingerprintSHA256(r.TLS.PeerCertificates[0])
	role, ok := s.policy.Lookup(fp)
	if !ok {
		return nil
	}
	return &Principal{Fingerprint: fp, Role: role, Authenticated: true}
}

// SetPrincipalResolver overrides how a caller's identity is determined.
func (s *Server) SetPrincipalResolver(fn PrincipalResolver) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.resolver = fn
}

// handler builds the canonical middleware chain (ADM-2.3).
//
// Order is a security property: Recoverer outermost so it also catches panics raised by inner
// middleware; Host before Origin because it is cheaper and rejects DNS rebinding before any
// origin parsing; CSRF before the router so no handler runs on a forged request.
func (s *Server) handler() http.Handler {
	var h http.Handler = s.router
	h = CSRFGuard(s.csrfToken())(h)
	h = OriginGuard(s.originsAllowlist())(h)
	h = HostGuard(s.hostsAllowlist(), s.scheme())(h)
	h = HandlerTimeout(DefaultHandlerTimeout)(h)
	h = BodyLimit(h)
	h = NoStoreAPI(h)
	h = SecurityHeaders(h)
	h = RequestID(h)
	return recoverPanics()(h)
}

// Addr reports the bound address.
func (s *Server) Addr() string {
	if s == nil {
		return ""
	}
	return s.addr
}

// Loopback reports whether the listener is bound to loopback.
func (s *Server) Loopback() bool {
	if s == nil {
		return false
	}
	return s.loopback
}

// Bus exposes the event bus.
func (s *Server) Bus() *EventBus {
	if s == nil {
		return nil
	}
	return s.bus
}

// Router exposes the router for tests and for later handler registration.
func (s *Server) Router() *Router {
	if s == nil {
		return nil
	}
	return s.router
}

// HasRealBuild reports whether a real console bundle is embedded.
func (s *Server) HasRealBuild() bool {
	if s == nil || s.static == nil {
		return false
	}
	return s.static.HasRealBuild()
}

// Start begins serving. It is idempotent-safe: a second call is an error rather than a
// second accept loop.
func (s *Server) Start() error {
	if s == nil {
		return errors.New("admin: nil server")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed.Load() {
		return errors.New("admin server already closed")
	}
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("admin server already started")
	}
	go func() {
		defer close(s.done)
		if err := s.http.Serve(s.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.mu.Lock()
			s.serveEr = err
			s.mu.Unlock()
		}
	}()
	return nil
}

// Shutdown drains the server. It is safe to call more than once.
func (s *Server) Shutdown(ctx context.Context) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	if s.closed.Load() {
		s.mu.Unlock()
		return nil
	}
	s.closed.Store(true)
	started := s.started.Load()
	s.mu.Unlock()

	// Closing the CSRF token makes every subsequent mutating request fail.
	s.closeCSRF()

	if !started {
		_ = s.listener.Close()
		return nil
	}
	err := s.http.Shutdown(ctx)
	<-s.done
	return err
}

// Err reports the serve error that ended the accept loop, if any.
func (s *Server) Err() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.serveEr
}

// recoverPanics converts a handler panic into a sanitized 500 rather than a dropped
// connection that leaks a goroutine dump.
func recoverPanics() Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					// The panic value may embed request data or paths; it is logged
					// server-side by the caller and never returned.
					WriteCoded(w, http.StatusInternalServerError, CodeInternal)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// scheme reports the serving scheme used for Host normalization.
func (s *Server) scheme() string {
	if s == nil || s.servingScheme == "" {
		return "http"
	}
	return s.servingScheme
}

// csrfToken exposes the per-boot token, for tests.
func (s *Server) csrfToken() *CSRFToken {
	if s == nil {
		return nil
	}
	return s.csrf
}

// hostsAllowlist and originsAllowlist expose the derived allowlists, for tests.
func (s *Server) hostsAllowlist() *HostAllowlist {
	if s == nil {
		return nil
	}
	return s.hosts
}

func (s *Server) originsAllowlist() *OriginAllowlist {
	if s == nil {
		return nil
	}
	return s.origins
}

// closeCSRF invalidates mutating access by retiring the token.
func (s *Server) closeCSRF() {
	if s == nil || s.csrf == nil {
		return
	}
	s.mu.Lock()
	s.csrf = nil
	s.mu.Unlock()
}
