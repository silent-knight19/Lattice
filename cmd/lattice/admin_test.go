package main

// Tests for SEC-6 (Lattice Console daemon wiring) and ADM-1 (admin server lifecycle).
//
// These are the tests SEC-6.5 calls for. Each one is written against the security property
// rather than the implementation: the loopback tests assert on the address the kernel
// actually bound, not on the flag that was passed.

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/metrics"
)

// baseCfg returns a Config with the admin console enabled on a free loopback port.
func baseCfg(t *testing.T) *Config {
	t.Helper()
	return &Config{
		Address:      "127.0.0.1:0",
		AdminAddress: "127.0.0.1:0",
	}
}

// SEC-6.1: no --admin-address means no server, and (nil, nil) rather than an error.
func TestNewAdminServerDisabledByDefault(t *testing.T) {
	var out bytes.Buffer
	srv, err := NewAdminServer(&Config{Address: "127.0.0.1:0"}, &out)
	if err != nil {
		t.Fatalf("disabled console must not error, got %v", err)
	}
	if srv != nil {
		t.Fatal("disabled console must return nil server")
	}
	if out.Len() != 0 {
		t.Errorf("disabled console must print nothing, got %q", out.String())
	}
	// Lifecycle methods must be no-ops on nil so the shutdown path needs no nil checks.
	if err := srv.Start(); err != nil {
		t.Errorf("nil Start: %v", err)
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("nil Shutdown: %v", err)
	}
	if srv.Addr() != "" || srv.Bus() != nil || srv.Err() != nil {
		t.Error("nil accessors must return zero values")
	}
}

// SEC-6.3: enabling the console emits a loud, unmissable warning.
func TestNewAdminServerWarnsOnEnable(t *testing.T) {
	var out bytes.Buffer
	srv, err := NewAdminServer(baseCfg(t), &out)
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	got := out.String()
	for _, want := range []string{"WARNING", "admin console ENABLED", srv.Addr(), "privileged"} {
		if !strings.Contains(got, want) {
			t.Errorf("startup output missing %q; got:\n%s", want, got)
		}
	}

	// The "build the frontend" hint tracks whether a real bundle is embedded. It is asserted
	// against the server's own state rather than hardcoded, because both states are correct
	// at different times: while web/dist held only the placeholder the hint had to appear,
	// and once FE-1 produces a real bundle it must NOT nag.
	if srv.HasRealBuild() {
		if strings.Contains(got, "npm run build") {
			t.Errorf("a real bundle is embedded, so the placeholder hint must be absent; got:\n%s", got)
		}
	} else if !strings.Contains(got, "npm run build") {
		t.Errorf("only a placeholder is embedded, so the build hint must appear; got:\n%s", got)
	}
}

// SEC-6.4: a non-loopback bind is refused unless BOTH opt-ins are present.
func TestNewAdminServerRefusesNonLoopback(t *testing.T) {
	tests := []struct {
		name    string
		cfg     *Config
		wantErr string
	}{
		{
			name:    "non-loopback without any opt-in",
			cfg:     &Config{Address: "127.0.0.1:0", AdminAddress: "0.0.0.0:0"},
			wantErr: "insecure-transport",
		},
		{
			name:    "insecure-transport alone is not sufficient",
			cfg:     &Config{Address: "127.0.0.1:0", AdminAddress: "0.0.0.0:0", InsecureTransport: true},
			wantErr: "admin-allow-remote",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := NewAdminServer(tc.cfg, &bytes.Buffer{})
			if err == nil {
				_ = srv.Shutdown(context.Background())
				t.Fatal("non-loopback bind must be refused")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error must mention %q; got %v", tc.wantErr, err)
			}
		})
	}
}

// SEC-6.4 positive case: both opt-ins permit a specific non-loopback address.
//
// A wildcard is still refused, so this binds a concrete non-loopback IP taken from this
// host's own interfaces, skipping when none exists.
func TestNewAdminServerAllowsRemoteWithBothOptIns(t *testing.T) {
	remote := nonLoopbackIPv4(t)

	var out bytes.Buffer
	cfg := &Config{
		Address:           "127.0.0.1:0",
		AdminAddress:      remote + ":0",
		InsecureTransport: true,
		AdminAllowRemote:  true,
	}
	srv, err := NewAdminServer(cfg, &out)
	if err != nil {
		t.Fatalf("both opt-ins should permit a non-loopback bind on %s: %v", remote, err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	// The warning must escalate, not stay quiet.
	if !strings.Contains(out.String(), "NON-LOOPBACK") {
		t.Errorf("remote bind must warn that it is network reachable; got:\n%s", out.String())
	}
}

// SEC-6.2/6.4: a wildcard bind is refused even with both opt-ins, because it would publish
// the console on every interface and yields no derivable Host allowlist.
func TestNewAdminServerRefusesWildcardEvenWithOptIns(t *testing.T) {
	cfg := &Config{
		Address:           "127.0.0.1:0",
		AdminAddress:      "0.0.0.0:0",
		InsecureTransport: true,
		AdminAllowRemote:  true,
	}
	srv, err := NewAdminServer(cfg, &bytes.Buffer{})
	if err == nil {
		_ = srv.Shutdown(context.Background())
		t.Fatal("wildcard bind must be refused even with both opt-ins")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error should identify the wildcard as the cause; got %v", err)
	}
}

// nonLoopbackIPv4 returns a usable non-loopback IPv4 address of this host, or skips.
func nonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok {
			continue
		}
		ip := ipnet.IP.To4()
		if ip != nil && !ip.IsLoopback() && !ip.IsUnspecified() {
			return ip.String()
		}
	}
	t.Skip("no non-loopback IPv4 interface available")
	return ""
}

// SEC-6.2: the bound socket must actually be loopback, verified after the fact.
func TestAdminServerBindsLoopbackOnly(t *testing.T) {
	var out bytes.Buffer
	srv, err := NewAdminServer(baseCfg(t), &out)
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	host, _, err := net.SplitHostPort(srv.Addr())
	if err != nil {
		t.Fatalf("Addr %q: %v", srv.Addr(), err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		t.Fatalf("admin bound non-loopback address %q", host)
	}
	if !strings.Contains(out.String(), "loopback only") {
		t.Errorf("loopback bind should be described as loopback only; got:\n%s", out.String())
	}
}

// SEC-6.5: a port conflict fails fast at construction, before Start.
func TestAdminServerPortConflictFailsFast(t *testing.T) {
	// Occupy a port.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = l.Close() }()
	port := l.Addr().(*net.TCPAddr).Port

	cfg := baseCfg(t)
	cfg.AdminAddress = fmt.Sprintf("127.0.0.1:%d", port)

	srv, err := NewAdminServer(cfg, &bytes.Buffer{})
	if err == nil {
		_ = srv.Shutdown(context.Background())
		t.Fatal("binding an occupied port must fail at construction")
	}
	if srv != nil {
		t.Error("failed construction must not return a server")
	}
}

// SEC-6.5: repeated starts are rejected rather than spawning a second accept loop.
func TestAdminServerRepeatedStartRejected(t *testing.T) {
	srv, err := NewAdminServer(baseCfg(t), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("first Start: %v", err)
	}
	if err := srv.Start(); err == nil {
		t.Error("second Start must be rejected")
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
}

// SEC-6.5: shutdown is idempotent and closes the port.
func TestAdminServerShutdownIdempotentAndReleasesPort(t *testing.T) {
	srv, err := NewAdminServer(baseCfg(t), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := srv.Addr()
	ctx := context.Background()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("first Shutdown: %v", err)
	}
	if err := srv.Shutdown(ctx); err != nil {
		t.Errorf("second Shutdown must be a no-op, got %v", err)
	}

	// The port must be free again once drained.
	l, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("port %s not released after shutdown: %v", addr, err)
	}
	_ = l.Close()
}

// SEC-6.5: after shutdown the port must not answer, proving the listener is really gone.
func TestAdminServerStopsServingAfterShutdown(t *testing.T) {
	srv, err := NewAdminServer(baseCfg(t), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	addr := srv.Addr()

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get("http://" + addr + "/")
	if err != nil {
		t.Fatalf("console should answer before shutdown: %v", err)
	}
	_ = resp.Body.Close()

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
	if _, err := client.Get("http://" + addr + "/"); err == nil {
		t.Error("console must not answer after shutdown")
	}
}

// renderedGauges reports which of the names appear in the registry's Prometheus output.
//
// Asserting on the rendered text rather than on an internal lookup exercises the same path
// an operator's scrape would, so a gauge that is registered but unrenderable is caught here.
func renderedGauges(t *testing.T, reg *metrics.Registry, names ...string) map[string]bool {
	t.Helper()
	var b bytes.Buffer
	if err := reg.WritePrometheus(&b); err != nil {
		t.Fatalf("WritePrometheus: %v", err)
	}
	out := map[string]bool{}
	for _, n := range names {
		out[n] = strings.Contains(b.String(), n)
	}
	return out
}

// SEC-6.5: the event bus gauges must register and unregister without colliding.
func TestAdminServerMetricsRegistration(t *testing.T) {
	const (
		dropped     = "lattice_admin_events_dropped"
		subscribers = "lattice_admin_subscribers"
	)
	reg := metrics.NewRegistry()
	srv, err := NewAdminServer(baseCfg(t), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()

	srv.RegisterMetrics(reg)
	for name, present := range renderedGauges(t, reg, dropped, subscribers) {
		if !present {
			t.Errorf("gauge %s not rendered after registration", name)
		}
	}

	srv.UnregisterMetrics(reg)
	for name, present := range renderedGauges(t, reg, dropped, subscribers) {
		if present {
			t.Errorf("gauge %s still rendered after UnregisterMetrics", name)
		}
	}
}

// SEC-6.5: registration twice must not panic on a duplicate-name error path.
func TestAdminServerMetricsRegistrationIdempotent(t *testing.T) {
	reg := metrics.NewRegistry()
	srv, err := NewAdminServer(baseCfg(t), &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewAdminServer: %v", err)
	}
	defer func() { _ = srv.Shutdown(context.Background()) }()
	srv.RegisterMetrics(reg)
	srv.RegisterMetrics(reg)
	srv.UnregisterMetrics(reg)
}

// SEC-6.5: metric registration on a nil server must be a no-op, not a panic.
func TestAdminServerNilSafeMetrics(t *testing.T) {
	var srv *AdminServer
	srv.RegisterMetrics(metrics.NewRegistry())
	srv.UnregisterMetrics(metrics.NewRegistry())
}

// ---------------------------------------------------------------------------
// SEC-6 flag validation (the ParseFlags layer, which is what operators actually hit).
// ---------------------------------------------------------------------------

// parseAdmin runs ParseFlags with the given arguments.
func parseAdmin(t *testing.T, args ...string) (*Config, error) {
	t.Helper()
	var so, se bytes.Buffer
	cfg, _, err := ParseFlags(args, &so, &se)
	return cfg, err
}

// SEC-6.1: the console is off unless --admin-address is given.
func TestAdminFlagDisabledByDefault(t *testing.T) {
	cfg, err := parseAdmin(t)
	if err != nil {
		t.Fatalf("default flags must be valid: %v", err)
	}
	if cfg.AdminAddress != "" {
		t.Errorf("console must be disabled by default, got AdminAddress=%q", cfg.AdminAddress)
	}
	if cfg.AdminAllowRemote {
		t.Error("AdminAllowRemote must default to false")
	}
}

// SEC-6.1/6.3: an explicit loopback address enables the console.
func TestAdminFlagLoopbackAccepted(t *testing.T) {
	cfg, err := parseAdmin(t, "--admin-address", "127.0.0.1:7070")
	if err != nil {
		t.Fatalf("loopback admin address should be accepted: %v", err)
	}
	if cfg.AdminAddress != "127.0.0.1:7070" {
		t.Errorf("AdminAddress = %q", cfg.AdminAddress)
	}
}

// SEC-6.2/6.4: the flag layer refuses a non-loopback bind without both opt-ins.
func TestAdminFlagNonLoopbackRequiresBothOptIns(t *testing.T) {
	// Each case asserts WHICH opt-in it is missing, so the test cannot pass for an
	// unrelated reason (for example an unrelated config error).
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			"no opt-ins",
			[]string{"--admin-address", "10.0.0.5:7070"},
			"--insecure-transport",
		},
		{
			"insecure-transport only",
			[]string{"--admin-address", "10.0.0.5:7070", "--insecure-transport"},
			"--admin-allow-remote",
		},
		{
			"admin-allow-remote only",
			[]string{"--admin-address", "10.0.0.5:7070", "--admin-allow-remote"},
			"--insecure-transport",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAdmin(t, tc.args...)
			if err == nil {
				t.Fatalf("non-loopback bind must be rejected with args %v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error should point at the missing opt-in %q; got %v", tc.wantErr, err)
			}
		})
	}
}

// SEC-6.4: with both opt-ins the flag layer accepts a non-loopback bind.
func TestAdminFlagNonLoopbackWithBothOptIns(t *testing.T) {
	cfg, err := parseAdmin(t,
		"--admin-address", "10.0.0.5:7070",
		"--insecure-transport",
		"--admin-allow-remote")
	if err != nil {
		t.Fatalf("both opt-ins should be accepted: %v", err)
	}
	if !cfg.AdminAllowRemote {
		t.Error("AdminAllowRemote should be true")
	}
}

// SEC-6.4: --admin-allow-remote with no --admin-address is a hard error, not a silent no-op.
func TestAdminFlagAllowRemoteWithoutAddress(t *testing.T) {
	_, err := parseAdmin(t, "--admin-allow-remote", "--insecure-transport")
	if err == nil {
		t.Fatal("--admin-allow-remote with no --admin-address must fail loudly")
	}
	if !strings.Contains(err.Error(), "admin-allow-remote") {
		t.Errorf("error should name the flag; got %v", err)
	}
}

// SEC-6.4: a wildcard admin bind is rejected even with both opt-ins.
func TestAdminFlagRejectsWildcard(t *testing.T) {
	_, err := parseAdmin(t,
		"--admin-address", "0.0.0.0:7070",
		"--insecure-transport",
		"--admin-allow-remote")
	if err == nil {
		t.Fatal("wildcard admin bind must be rejected")
	}
	if !strings.Contains(err.Error(), "wildcard") {
		t.Errorf("error should identify the wildcard; got %v", err)
	}
}

// SEC-6: a malformed admin address is rejected at the flag layer.
func TestAdminFlagRejectsMalformedAddress(t *testing.T) {
	for _, addr := range []string{"127.0.0.1", "127.0.0.1:notaport", "127.0.0.1:99999"} {
		if _, err := parseAdmin(t, "--admin-address", addr); err == nil {
			t.Errorf("malformed admin address %q must be rejected", addr)
		}
	}
}

// SEC-6: the admin port must not collide with the data, metrics or pprof ports.
func TestAdminFlagRejectsPortCollisions(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			"data port",
			[]string{"--address", "127.0.0.1:7070", "--admin-address", "127.0.0.1:7070"},
			"--address",
		},
		{
			"metrics port",
			[]string{"--metrics-address", "127.0.0.1:7071", "--admin-address", "127.0.0.1:7071"},
			"--metrics-address",
		},
		{
			"pprof port",
			[]string{"--pprof-address", "127.0.0.1:7072", "--admin-address", "127.0.0.1:7072"},
			"--pprof-address",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parseAdmin(t, tc.args...)
			if err == nil {
				t.Fatalf("port collision must be rejected: %v", tc.args)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error should name the conflicting listener %q; got %v", tc.wantErr, err)
			}
		})
	}
}

// SEC-6: supplying the policy both inline and by file is ambiguous and must be rejected.
func TestAdminFlagRejectsBothPolicySources(t *testing.T) {
	_, err := parseAdmin(t,
		"--admin-address", "127.0.0.1:7070",
		"--admin-authz-policy", "AA=admin",
		"--admin-authz-policy-file", "policy.json")
	if err == nil {
		t.Fatal("inline and file policy must be mutually exclusive")
	}
}
