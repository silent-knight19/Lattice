package admin

import (
	"net"
	"strings"
	"testing"
)

// firstNonLoopbackIPv4 returns an address of a local, non-loopback IPv4 interface, or ""
// when the host has none. Tests use it so that binding a specific remote address is
// exercised for real rather than failing in the kernel.
func firstNonLoopbackIPv4(t *testing.T) string {
	t.Helper()
	ifaces, err := net.Interfaces()
	if err != nil {
		t.Skipf("cannot enumerate interfaces: %v", err)
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP.To4()
			if ip == nil || ip.IsLoopback() || ip.IsUnspecified() {
				continue
			}
			return ip.String()
		}
	}
	t.Skip("host has no non-loopback IPv4 interface")
	return ""
}

// TestBindLoopback_PreBindEnforcement verifies subcheck 1: a non-loopback address is
// refused unless the operator explicitly opted in.
func TestBindLoopback_PreBindEnforcement(t *testing.T) {
	remote := firstNonLoopbackIPv4(t)

	tests := []struct {
		name        string
		addr        string
		allowRemote bool
		wantErr     bool
		errContains string
	}{
		{"loopback v4 allowed", "127.0.0.1:0", false, false, ""},
		{"loopback v6 allowed", "[::1]:0", false, false, ""},
		{"localhost allowed", "localhost:0", false, false, ""},
		{"non loopback refused by default", remote + ":0", false, true, "loopback"},
		{"wildcard refused by default", "0.0.0.0:0", false, true, "loopback"},
		{"non loopback allowed with opt-in", remote + ":0", true, false, ""},
		{"malformed addr", "no-port", false, true, "expected host:port"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			res, err := BindLoopback(BindOptions{Addr: tc.addr, AllowRemote: tc.allowRemote, Scheme: "http"})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("BindLoopback(%q, allowRemote=%v) = nil error, want error", tc.addr, tc.allowRemote)
				}
				if !strings.Contains(err.Error(), tc.errContains) {
					t.Errorf("error %q does not contain %q", err, tc.errContains)
				}
				return
			}
			if err != nil {
				t.Fatalf("BindLoopback(%q) unexpected error: %v", tc.addr, err)
			}
			defer func() { _ = res.Listener.Close() }()
		})
	}
}

// TestBindLoopback_WildcardAlwaysRefused verifies that even an explicit remote opt-in
// cannot produce a wildcard bind. A wildcard exposes the console on every interface, is
// never what the operator intended, and yields no derivable Host allowlist.
func TestBindLoopback_WildcardAlwaysRefused(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:0", "[::]:0"} {
		t.Run(addr, func(t *testing.T) {
			res, err := BindLoopback(BindOptions{Addr: addr, AllowRemote: true, Scheme: "http"})
			if err == nil {
				_ = res.Listener.Close()
				t.Fatalf("BindLoopback(%q, AllowRemote=true) succeeded; wildcard bind must always be refused", addr)
			}
			if !strings.Contains(err.Error(), "wildcard") {
				t.Errorf("error %q should mention wildcard", err)
			}
		})
	}
}

// TestBindLoopback_PostBindVerification verifies subcheck 3: the allowlists are derived
// from what the kernel actually bound, and Loopback accurately reports reality.
func TestBindLoopback_PostBindVerification(t *testing.T) {
	res, err := BindLoopback(BindOptions{Addr: "127.0.0.1:0", Scheme: "http"})
	if err != nil {
		t.Fatalf("BindLoopback: %v", err)
	}
	defer func() { _ = res.Listener.Close() }()

	if !res.Loopback {
		t.Error("Loopback = false for a loopback bind")
	}
	if res.Listener == nil || res.Listener.Addr() == nil {
		t.Fatal("listener or Addr is nil")
	}
	// Port 0 means the kernel picked an ephemeral port; the derived allowlist must use
	// that real port, not the requested "0".
	_, port, _ := net.SplitHostPort(res.Addr)
	if port == "0" || port == "" {
		t.Errorf("derived addr %q has no real port", res.Addr)
	}
	if !res.Hosts.Allows("127.0.0.1:"+port, "http") {
		t.Errorf("derived HostAllowlist does not allow the actually-bound %q", res.Addr)
	}
	if !res.Hosts.Allows("localhost:"+port, "http") {
		t.Error("derived HostAllowlist does not allow localhost alias")
	}
	if !res.Origins.Allows("http://127.0.0.1:" + port) {
		t.Errorf("derived OriginAllowlist does not allow http://127.0.0.1:%s", port)
	}
	// The allowlist must be derived from the bound address, not from something looser.
	if res.Hosts.Allows("0.0.0.0:"+port, "http") {
		t.Error("derived HostAllowlist unexpectedly allows the wildcard host")
	}
}

// TestBindLoopback_DefaultPortElision verifies the allowlists work when the listener is on
// a scheme default port, where the Host/Origin may legitimately omit it.
func TestBindLoopback_DefaultPortElision(t *testing.T) {
	// Port 80 requires privileges on most systems; use the allowlist constructors
	// directly instead, which is where the normalization lives.
	h := NewHostAllowlistScheme("http", SelfHosts("127.0.0.1:80", "http")...)
	if !h.Allows("127.0.0.1", "http") {
		t.Error("Host '127.0.0.1' should be allowed when serving on port 80")
	}
	o := NewOriginAllowlist(SelfOrigins("127.0.0.1:80", "http")...)
	if !o.Allows("http://127.0.0.1") {
		t.Error("Origin 'http://127.0.0.1' should be allowed when serving on port 80")
	}
}
