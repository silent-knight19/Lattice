package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestParseHostHeader covers normalization and the malformed shapes an attacker controls.
func TestParseHostHeader(t *testing.T) {
	tests := []struct {
		name     string
		raw      string
		scheme   string
		wantOK   bool
		wantHost string
		wantPort string
	}{
		{"ipv4 with port", "127.0.0.1:7070", "http", true, "127.0.0.1", "7070"},
		{"ipv6 with port", "[::1]:7070", "http", true, "::1", "7070"},
		{"localhost with port", "localhost:7070", "http", true, "localhost", "7070"},
		{"host case normalized", "LOCALHOST:7070", "http", true, "localhost", "7070"},
		{"default port elided", "example.com", "http", true, "example.com", "80"},
		{"https default elided", "example.com", "https", true, "example.com", "443"},
		{"explicit default port", "example.com:80", "http", true, "example.com", "80"},

		{"empty", "", "http", false, "", ""},
		{"userinfo", "user@evil.com", "http", false, "", ""},
		{"password in host", "user:pass@evil.com", "http", false, "", ""},
		{"path", "evil.com/path", "http", false, "", ""},
		{"query", "evil.com?a=b", "http", false, "", ""},
		{"fragment", "evil.com#x", "http", false, "", ""},
		{"backslash", "evil.com\\x", "http", false, "", ""},
		{"trailing dot", "localhost.:7070", "http", false, "", ""},
		{"empty port", "example.com:", "http", false, "", ""},
		{"non numeric port", "example.com:abc", "http", false, "", ""},
		{"port too large", "example.com:99999", "http", false, "", ""},
		{"embedded space", "evil .com", "http", false, "", ""},
		{"embedded crlf", "evil.com\r\nX: y", "http", false, "", ""},
		{"bare ipv6 unbracketed", "::1", "http", false, "", ""},
		{"unterminated bracket", "[::1:7070", "http", false, "", ""},
		{"overlong", strings.Repeat("a", 600), "http", false, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseHostHeader(tc.raw, tc.scheme)
			if ok != tc.wantOK {
				t.Fatalf("parseHostHeader(%q) ok = %v, want %v (got %+v)", tc.raw, ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.host != tc.wantHost || got.port != tc.wantPort {
				t.Errorf("parseHostHeader(%q) = %s|%s, want %s|%s", tc.raw, got.host, got.port, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// TestSelfHosts covers derivation of the Host allowlist from a bound address.
func TestSelfHosts(t *testing.T) {
	tests := []struct {
		name   string
		addr   string
		scheme string
		want   []string
	}{
		{"loopback v4 includes localhost alias", "127.0.0.1:7070", "http",
			[]string{"127.0.0.1:7070", "localhost:7070"}},
		{"loopback v6 includes localhost alias", "[::1]:7070", "http",
			[]string{"[::1]:7070", "localhost:7070"}},
		{"default port drops port", "127.0.0.1:80", "http", []string{"127.0.0.1"}},
		{"wildcard yields nothing", "0.0.0.0:7070", "http", nil},
		{"wildcard v6 yields nothing", "[::]:7070", "http", nil},
		{"non loopback single host", "10.0.0.5:7070", "http", []string{"10.0.0.5:7070"}},
		{"malformed addr", "nonsense", "http", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SelfHosts(tc.addr, tc.scheme)
			if len(got) != len(tc.want) {
				t.Fatalf("SelfHosts(%q) = %v, want %v", tc.addr, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("SelfHosts[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
			for _, h := range got {
				if _, ok := parseHostHeader(h, tc.scheme); !ok {
					t.Errorf("SelfHosts produced unparseable host %q", h)
				}
			}
		})
	}
}

// TestHostGuard_DNSRebinding is the central SEC-2 assertion: a rebound request whose Host
// is the attacker's own domain must never reach the handler, even though its Origin is
// self-consistent and would be indistinguishable from legitimate traffic to OriginGuard.
func TestHostGuard_DNSRebinding(t *testing.T) {
	// Attacker controls attacker.example, which resolves to 127.0.0.1.
	// Origin looks entirely self-consistent, so OriginGuard alone would allow it.
	const attackerHost = "attacker.example"
	const attackerOrigin = "http://attacker.example"

	originGuard := OriginGuard(NewOriginAllowlist(attackerOrigin))
	hosts := NewHostAllowlist(SelfHosts("127.0.0.1:7070", "http")...)

	var executed atomic.Int64
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executed.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	h := HostGuard(hosts, "http")(originGuard(inner))

	// Sanity: the Origin guard on its own would NOT stop this.
	only := OriginGuard(NewOriginAllowlist(attackerOrigin))(inner)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/crash", strings.NewReader("x"))
	req.Host = attackerHost
	req.Header.Set("Origin", attackerOrigin)
	only.ServeHTTP(rec, req)
	if executed.Load() != 1 {
		t.Fatalf("precondition failed: Origin-only guard should let a self-consistent origin through (got %d)", executed.Load())
	}
	executed.Store(0)

	// With HostGuard in front, the same request is refused.
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodPost, "/api/v1/crash", strings.NewReader("x"))
	req.Host = attackerHost
	req.Header.Set("Origin", attackerOrigin)
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	if executed.Load() != 0 {
		t.Errorf("SECURITY: handler executed for rebound Host %q", attackerHost)
	}
}

// TestHostGuard_AttackCases covers the full decision table.
func TestHostGuard_AttackCases(t *testing.T) {
	allow := NewHostAllowlist(SelfHosts("127.0.0.1:7070", "http")...)

	tests := []struct {
		name       string
		host       string
		wantStatus int
		wantRun    bool
	}{
		{"exact loopback host", "127.0.0.1:7070", http.StatusOK, true},
		{"localhost alias", "localhost:7070", http.StatusOK, true},
		{"case-insensitive", "LOCALHOST:7070", http.StatusOK, true},

		{"DNS rebinding host", "attacker.example", http.StatusBadRequest, false},
		{"wrong port", "127.0.0.1:9999", http.StatusBadRequest, false},
		{"port omitted", "127.0.0.1", http.StatusBadRequest, false},
		{"scheme in host", "http://127.0.0.1:7070", http.StatusBadRequest, false},
		{"prefix confusion", "127.0.0.1:7070.evil.com", http.StatusBadRequest, false},
		{"suffix confusion", "evil-127.0.0.1:7070", http.StatusBadRequest, false},
		{"ipv6 not in allowlist", "[::1]:7070", http.StatusBadRequest, false},
		{"userinfo", "127.0.0.1:7070@evil.com", http.StatusBadRequest, false},
		{"trailing dot", "localhost.:7070", http.StatusBadRequest, false},
		{"empty host", "", http.StatusBadRequest, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var executed atomic.Int64
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				executed.Add(1)
				w.WriteHeader(http.StatusOK)
			})
			h := HostGuard(allow, "http")(inner)

			req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
			req.Host = tc.host
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != tc.wantStatus {
				t.Errorf("status = %d, want %d", rec.Code, tc.wantStatus)
			}
			if tc.wantRun && executed.Load() != 1 {
				t.Errorf("handler executed %d times, want 1", executed.Load())
			}
			if !tc.wantRun && executed.Load() != 0 {
				t.Errorf("SECURITY: handler executed for Host %q", tc.host)
			}
		})
	}
}

// TestHostGuard_RejectionLeaksNothing verifies the 400 body reveals no configuration.
func TestHostGuard_RejectionLeaksNothing(t *testing.T) {
	allow := NewHostAllowlist("127.0.0.1:7070")
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := HostGuard(allow, "http")(inner)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Host = "attacker.example"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	for _, secret := range []string{"127.0.0.1", "7070", "localhost", "attacker"} {
		if strings.Contains(body, secret) {
			t.Errorf("400 body leaks %q: %s", secret, body)
		}
	}
	if hasAnyCORSHeader(rec.Header()) {
		t.Error("400 response carries a CORS header")
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff")
	}
	if rec.Header().Get("Cache-Control") != "no-store" {
		t.Error("missing no-store")
	}
}

// TestHostAllowlist_FailClosed verifies a nil or empty allowlist denies everything,
// including a well-formed Host.
func TestHostAllowlist_FailClosed(t *testing.T) {
	var nilList *HostAllowlist
	if nilList.Allows("127.0.0.1:7070", "http") {
		t.Error("nil allowlist must deny")
	}
	if NewHostAllowlist().Allows("127.0.0.1:7070", "http") {
		t.Error("empty allowlist must deny")
	}
	// A malformed configured entry must be dropped, not silently accepted.
	mixed := NewHostAllowlist("127.0.0.1:7070", "user@evil.com", "")
	if !mixed.Allows("127.0.0.1:7070", "http") {
		t.Error("valid entry should work")
	}
	if mixed.Allows("user@evil.com", "http") || mixed.Allows("", "http") {
		t.Error("malformed entries must be dropped")
	}
}
