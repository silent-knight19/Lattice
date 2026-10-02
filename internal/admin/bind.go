package admin

import (
	"fmt"
	"net"
)

// BindOptions describes how the admin listener may be bound.
type BindOptions struct {
	// Addr is the address to bind, as "host:port".
	Addr string
	// AllowRemote permits binding a non-loopback address. It requires an explicit
	// operator opt-in (see SEC-6) because the console exposes engine internals.
	AllowRemote bool
	// Scheme is the serving scheme ("http" or "https"), used to derive the Host and
	// Origin allowlists.
	Scheme string
}

// BindResult reports the address actually bound plus the derived security allowlists.
type BindResult struct {
	Listener net.Listener
	// Addr is the address the kernel bound, which may differ from the requested one
	// (notably when port 0 was requested and an ephemeral port was assigned).
	Addr string
	// Hosts is the derived Host allowlist.
	Hosts *HostAllowlist
	// Origins is the derived Origin allowlist.
	Origins *OriginAllowlist
	// Loopback reports whether the listener is bound to a loopback interface.
	Loopback bool
}

// BindLoopback validates the requested address, binds it, and verifies the result.
//
// It mirrors the fail-fast, synchronously-bound pattern of cmd/lattice/pprof.go and
// internal/metrics/server.go, and applies three checks in order:
//
//  1. Pre-bind: a non-loopback address is refused unless AllowRemote is set.
//  2. Bind synchronously, so a port conflict fails here rather than at Start().
//  3. Post-bind: verify what the kernel actually bound. Configuration can lie; the
//     listener's own Addr() cannot. This is the check that catches a wildcard bind
//     sneaking through when loopback was required.
//
// The derived allowlists are computed from the address the kernel returned, never from
// the requested string, so a request can only be accepted under a Host that genuinely
// reaches this server.
func BindLoopback(opts BindOptions) (*BindResult, error) {
	scheme := opts.Scheme
	if scheme != "https" {
		scheme = "http"
	}

	// Pre-bind loopback enforcement.
	host := ""
	if h, _, err := net.SplitHostPort(opts.Addr); err == nil {
		host = h
	} else {
		return nil, fmt.Errorf("admin server error: invalid address %q (expected host:port): %w", opts.Addr, err)
	}

	isLoop := false
	if ip := net.ParseIP(host); ip != nil {
		isLoop = ip.IsLoopback()
	} else {
		switch host {
		case "localhost", "ip6-localhost", "ip6-loopback":
			isLoop = true
		}
	}
	if !isLoop && !opts.AllowRemote {
		return nil, fmt.Errorf("admin server error: address %q must be a loopback interface "+
			"(127.0.0.1, ::1, localhost); remote binding requires an explicit opt-in", opts.Addr)
	}

	ln, err := net.Listen("tcp", opts.Addr)
	if err != nil {
		return nil, fmt.Errorf("admin server error: failed to listen on %s: %w", opts.Addr, err)
	}

	// Post-bind verification: trust the kernel's view, not the configuration's.
	boundLoopback := false
	boundHost := ""
	if tcpAddr, ok := ln.Addr().(*net.TCPAddr); ok {
		boundLoopback = tcpAddr.IP.IsLoopback()
		boundHost = tcpAddr.IP.String()
	} else {
		_ = ln.Close()
		return nil, fmt.Errorf("admin server error: listener address is not TCP")
	}

	// A wildcard bind slipped through while loopback was required.
	if isLoop && !boundLoopback {
		_ = ln.Close()
		return nil, fmt.Errorf("admin server error: requested loopback address %q but bound to %s", opts.Addr, boundHost)
	}
	// Even with AllowRemote, refuse a wildcard bind: it exposes the console on every
	// interface, which is never what an operator enabling a remote bind intends, and it
	// yields no derivable Host allowlist.
	if !boundLoopback && isUnspecified(boundHost) {
		_ = ln.Close()
		return nil, fmt.Errorf("admin server error: wildcard bind (%s) is not permitted for the admin server; "+
			"bind a specific address", boundHost)
	}

	boundAddr := ln.Addr().String()
	schemeForHosts := scheme
	// net.Listener renders IPv6 as [::1]:port, which SelfHosts handles.
	hostVal := SelfHosts(boundAddr, schemeForHosts)
	originVals := SelfOrigins(boundAddr, schemeForHosts)

	return &BindResult{
		Listener: ln,
		Addr:     boundAddr,
		Hosts:    NewHostAllowlistScheme(schemeForHosts, hostVal...),
		Origins:  NewOriginAllowlist(originVals...),
		Loopback: boundLoopback,
	}, nil
}

// isUnspecified reports whether a textual IP is a wildcard address (0.0.0.0 or ::).
func isUnspecified(host string) bool {
	ip := net.ParseIP(host)
	return ip != nil && ip.IsUnspecified()
}
