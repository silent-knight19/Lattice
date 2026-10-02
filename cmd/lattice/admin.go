package main

// This file wires the Lattice Console admin API into the daemon lifecycle (SEC-6 / ADM-7).
//
// The console is OFF by default: with no --admin-address there is no listener, no
// goroutine and no allocation. Everything below is gated on that one condition, and
// NewAdminServer returns (nil, nil) when disabled so the caller's shutdown path does not
// need to re-test the configuration.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"

	"github.com/silent-knight19/lattice/internal/admin"
	"github.com/silent-knight19/lattice/internal/metrics"
	"github.com/silent-knight19/lattice/internal/transport"
)

// AdminServer bundles the admin API with the handles the daemon needs to manage it.
type AdminServer struct {
	server *admin.Server
	bus    *admin.EventBus
	addr   string
}

// NewAdminServer constructs the admin server for cfg, or returns (nil, nil) when disabled.
//
// It binds synchronously so a port conflict surfaces here, at initialization, rather than
// after the engine is already running.
func NewAdminServer(cfg *Config, stdout io.Writer) (*AdminServer, error) {
	if cfg == nil || cfg.AdminAddress == "" {
		return nil, nil
	}

	// Resolve the authorization policy before binding, so a bad policy fails fast at
	// startup instead of on the first request.
	var policy *transport.AuthzPolicy
	switch {
	case cfg.AdminAuthzPolicyFile != "":
		p, err := transport.LoadAuthzPolicyFile(cfg.AdminAuthzPolicyFile)
		if err != nil {
			return nil, fmt.Errorf("admin server error: loading --admin-authz-policy-file: %w", err)
		}
		policy = p
	case len(cfg.AdminAuthzPolicy) > 0:
		raw, err := json.Marshal(cfg.AdminAuthzPolicy)
		if err != nil {
			return nil, fmt.Errorf("admin server error: encoding --admin-authz-policy: %w", err)
		}
		p, err := transport.ParseAuthzPolicyJSON(raw)
		if err != nil {
			return nil, fmt.Errorf("admin server error: parsing --admin-authz-policy: %w", err)
		}
		policy = p
	}

	// Require BOTH opt-ins here as well as in config validation. Defence in depth: this
	// function is reachable from tests and future call sites that bypass flag parsing.
	allowRemote := cfg.AdminAllowRemote && cfg.InsecureTransport
	if !allowRemote && !transport.IsLoopbackAddress(hostOf(cfg.AdminAddress)) {
		return nil, fmt.Errorf("admin server error: refusing non-loopback --admin-address %q "+
			"without both --insecure-transport and --admin-allow-remote", cfg.AdminAddress)
	}

	srv, err := admin.NewServer(admin.ServerOptions{
		Bind: admin.BindOptions{
			Addr:        cfg.AdminAddress,
			AllowRemote: allowRemote,
			Scheme:      "http",
		},
		Policy: policy,
	})
	if err != nil {
		return nil, err
	}

	as := &AdminServer{server: srv, bus: srv.Bus(), addr: srv.Addr()}

	// SEC-6.3: a loud warning whenever the console is enabled, because enabling this
	// surface is a deliberate act and an operator who forgot must be told immediately.
	scope := "loopback only"
	if !srv.Loopback() {
		scope = "NON-LOOPBACK: reachable from other hosts on the network"
	}
	fmt.Fprintf(stdout,
		"lattice: WARNING: admin console ENABLED on http://%s (%s)\n"+
			"lattice: WARNING: it exposes storage-engine internals and can mutate state; "+
			"treat access to this port as privileged\n",
		as.addr, scope)

	if !srv.HasRealBuild() {
		fmt.Fprintf(stdout,
			"lattice: note: console assets are the committed placeholder; "+
				"run `cd web && npm ci && npm run build` to embed the real UI\n")
	}

	return as, nil
}

// hostOf returns the host portion of addr, or "" when it is malformed. A malformed address
// has already been rejected by config validation, so "" here is safe to treat as non-loopback.
func hostOf(addr string) string {
	h, _, err := net.SplitHostPort(addr)
	if err != nil {
		return ""
	}
	return h
}

// Addr reports the bound admin address.
func (a *AdminServer) Addr() string {
	if a == nil {
		return ""
	}
	return a.addr
}

// Bus exposes the event bus so the daemon can publish Raft and compaction events.
func (a *AdminServer) Bus() *admin.EventBus {
	if a == nil {
		return nil
	}
	return a.bus
}

// HasRealBuild reports whether a real (non-placeholder) console bundle is embedded.
//
// It is the same signal that decides whether the startup hint is printed, so tests and
// future callers can reason about it rather than parsing log output.
func (a *AdminServer) HasRealBuild() bool {
	if a == nil {
		return false
	}
	return a.server.HasRealBuild()
}

// Start begins serving.
func (a *AdminServer) Start() error {
	if a == nil {
		return nil
	}
	return a.server.Start()
}

// Shutdown drains the console within ctx. Safe on a nil receiver and safe to call twice.
func (a *AdminServer) Shutdown(ctx context.Context) error {
	if a == nil {
		return nil
	}
	return a.server.Shutdown(ctx)
}

// Err reports the serve error that ended the accept loop, if any.
func (a *AdminServer) Err() error {
	if a == nil {
		return nil
	}
	return a.server.Err()
}

// RegisterMetrics exposes console gauges under stable names.
//
// Registration happens here, at wiring time, rather than inside the admin package, so the
// process-wide registry is only touched by the process that owns it.
func (a *AdminServer) RegisterMetrics(reg *metrics.Registry) {
	if a == nil || reg == nil || a.bus == nil {
		return
	}
	bus := a.bus
	reg.RegisterGaugeFunc(admin.AdminEventsDroppedMetric, "Events dropped because a console subscriber was not keeping up", nil,
		func() int64 { return int64(bus.Dropped()) })
	reg.RegisterGaugeFunc(admin.AdminSubscribersMetric, "Currently connected console event-stream subscribers", nil,
		func() int64 { return int64(bus.Subscribers()) })
}

// UnregisterMetrics removes the gauges on shutdown.
func (a *AdminServer) UnregisterMetrics(reg *metrics.Registry) {
	if a == nil || reg == nil {
		return
	}
	reg.UnregisterGaugeFunc(admin.AdminEventsDroppedMetric)
	reg.UnregisterGaugeFunc(admin.AdminSubscribersMetric)
}
