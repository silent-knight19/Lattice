package admin

import (
	"net/http"
	"time"
)

// This file holds the http.Server timeout configuration (SEC-7.2).
//
// The values follow internal/metrics/server.go:168-175 and cmd/lattice/pprof.go:83-88,
// with the one deliberate divergence documented on WriteTimeout.

// ServerTimeouts returns the hardened timeout set for the admin http.Server.
//
// Rationale per field:
//
//   - ReadHeaderTimeout 5s: bounds the Slowloris window. Without it a client can hold a
//     connection open indefinitely by dribbling headers. This is the single most
//     important field for a loopback service that a hostile page can reach.
//   - ReadTimeout 15s: bounds header+body arrival. Longer than ReadHeaderTimeout because a
//     legitimate 4 MiB key/value write needs time to transfer.
//   - WriteTimeout 0: MUST be zero (disabled) because the SSE endpoint at /api/v1/events is
//     a long-lived stream. A non-zero WriteTimeout would sever every SSE connection after
//     that duration. Per-request deadlines are applied by HandlerTimeout instead, which
//     exempts streaming paths.
//   - IdleTimeout 30s: reclaims keep-alive connections. Matches the metrics server.
//   - MaxHeaderBytes 1 MiB: matches the existing servers. Header size is already bounded;
//     SEC-7's concern is the BODY, which MaxBytesReader handles.
func ServerTimeouts() ServerTimeoutConfig {
	return ServerTimeoutConfig{
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      0, // SSE: disabled by design; see HandlerTimeout
		IdleTimeout:       30 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// ServerTimeoutConfig holds the timeout set for an http.Server.
type ServerTimeoutConfig struct {
	ReadHeaderTimeout time.Duration
	ReadTimeout       time.Duration
	WriteTimeout      time.Duration
	IdleTimeout       time.Duration
	MaxHeaderBytes    int
}

// ApplyTo configures a server with these timeouts.
func (c ServerTimeoutConfig) ApplyTo(srv *http.Server) {
	if srv == nil {
		return
	}
	srv.ReadHeaderTimeout = c.ReadHeaderTimeout
	srv.ReadTimeout = c.ReadTimeout
	srv.WriteTimeout = c.WriteTimeout
	srv.IdleTimeout = c.IdleTimeout
	srv.MaxHeaderBytes = c.MaxHeaderBytes
}

// NewHTTPServer builds an *http.Server with the hardened timeout set applied.
func NewHTTPServer(handler http.Handler) *http.Server {
	srv := &http.Server{Handler: handler}
	ServerTimeouts().ApplyTo(srv)
	return srv
}
