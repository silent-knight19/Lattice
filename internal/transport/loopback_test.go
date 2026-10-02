package transport

import (
	stdErrors "errors"
	"testing"

	"github.com/silent-knight19/lattice/internal/errors"
)

// =============================================================================
// Loopback classification (regression).
//
// Every security gate in this package keys off IsLoopbackAddress: a bind not
// recognized as loopback must present TLS 1.3 mTLS and an authorization policy.
// The bare hostnames "pipe" and "local" were previously classified as loopback, so
// an address like "local:9099" skipped those requirements entirely and a Server was
// constructed with no TLS at all. Neither word is a loopback address on any OS.
// =============================================================================

func TestLoopback_Classification(t *testing.T) {
	cases := []struct {
		addr string
		want bool
		why  string
	}{
		// Genuine loopback.
		{"127.0.0.1:9099", true, "IPv4 loopback literal"},
		{"127.0.0.2:9099", true, "whole 127.0.0.0/8 is loopback, not just .1"},
		{"127.1.2.3:9099", true, "whole 127.0.0.0/8 is loopback"},
		{"[::1]:9099", true, "IPv6 loopback literal"},
		{"::1", true, "bare IPv6 loopback, no port"},
		{"localhost:9099", true, "well-known loopback alias"},
		{"LOCALHOST:9099", true, "alias match is case-insensitive"},
		{"[::1]:9099", true, "brackets trimmed before comparison"},

		// Must NOT be loopback.
		{"", false, "empty address"},
		{"0.0.0.0:9099", false, "wildcard is not loopback"},
		{"[::]:9099", false, "IPv6 wildcard is not loopback"},
		{"10.0.0.5:9099", false, "routable IPv4"},
		{"192.168.1.10:9099", false, "private but routable"},
		{"172.16.0.1:9099", false, "private but routable"},
		{"8.8.8.8:9099", false, "public IPv4"},
		{"[2001:db8::1]:9099", false, "public IPv6"},
		{"example.com:9099", false, "arbitrary hostname is not trusted as loopback"},
		{"127.0.0.1.example.com:9099", false, "hostname merely containing a loopback literal"},

		// The regression: these are not loopback addresses.
		{"pipe:9099", false, "\"pipe\" is not a loopback interface on any OS"},
		{"local:9099", false, "\"local\" is not a loopback interface on any OS"},
		{"pipe", false, "bare \"pipe\""},
		{"local", false, "bare \"local\""},
		{"PIPE:9099", false, "the word is matched case-insensitively and still is not loopback"},
	}

	for _, tc := range cases {
		t.Run(tc.addr, func(t *testing.T) {
			if got := IsLoopbackAddress(tc.addr); got != tc.want {
				t.Errorf("IsLoopbackAddress(%q) = %v; want %v (%s)", tc.addr, got, tc.want, tc.why)
			}
		})
	}
}

// TestLoopback_MagicHostnameFailsClosedWithoutTLS is the security-level assertion:
// the original finding was not that a helper misclassified a word, but that the
// misclassification let a Server come up with no TLS whatsoever.
func TestLoopback_MagicHostnameFailsClosedWithoutTLS(t *testing.T) {
	for _, addr := range []string{"local:9099", "pipe:9099", "PIPE:9099"} {
		t.Run(addr, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.Address = addr
			// No TLS, and InsecureTransport deliberately left false.
			srv, err := NewServer(cfg, newCountingEngine())
			if err == nil {
				_ = srv.Close()
				t.Fatalf("NewServer accepted %q with no TLS and InsecureTransport=false; "+
					"a non-loopback bind must fail closed with ErrInsecureTransport", addr)
			}
			if !stdErrors.Is(err, errors.ErrInsecureTransport) {
				t.Fatalf("NewServer(%q) returned %v; want ErrInsecureTransport", addr, err)
			}
		})
	}
}

// TestLoopback_LoopbackBindStillExempt confirms the fix did not over-correct: a
// genuine loopback bind must remain exempt from the TLS requirement, otherwise
// local development and test automation break.
func TestLoopback_LoopbackBindStillExempt(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:0", "localhost:0", "[::1]:0"} {
		t.Run(addr, func(t *testing.T) {
			cfg := DefaultServerConfig()
			cfg.Address = addr
			srv, err := NewServer(cfg, newCountingEngine())
			if err != nil {
				t.Fatalf("NewServer(%q) with no TLS returned %v; a genuine loopback bind "+
					"must remain exempt from the TLS requirement", addr, err)
			}
			_ = srv.Close()
		})
	}
}

// TestLoopback_ExplicitInsecureTransportStillHonored confirms the documented opt-in
// path is unaffected: a non-loopback bind with InsecureTransport=true is still the
// operator's deliberate choice, not an accidental bypass.
func TestLoopback_ExplicitInsecureTransportStillHonored(t *testing.T) {
	cfg := DefaultServerConfig()
	cfg.Address = "local:0"
	cfg.InsecureTransport = true
	srv, err := NewServer(cfg, newCountingEngine())
	if err != nil {
		t.Fatalf("NewServer with explicit InsecureTransport returned %v; the documented "+
			"opt-in must keep working", err)
	}
	_ = srv.Close()
}
