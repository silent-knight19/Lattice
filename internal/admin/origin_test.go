package admin

import "testing"

// TestParseOrigin covers normalization and, more importantly, every malformed shape an
// attacker might supply. A permissive case here is a console bypass.
func TestParseOrigin(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		wantOK     bool
		wantScheme string
		wantHost   string
		wantPort   string
	}{
		// --- Accept: well-formed origins ---
		{"loopback with port", "http://127.0.0.1:7070", true, "http", "127.0.0.1", "7070"},
		{"ipv6 loopback with port", "http://[::1]:7070", true, "http", "::1", "7070"},
		{"localhost with port", "http://localhost:7070", true, "http", "localhost", "7070"},
		{"https scheme", "https://console.example:8443", true, "https", "console.example", "8443"},
		{"default http port elided", "http://example.com", true, "http", "example.com", "80"},
		{"default https port elided", "https://example.com", true, "https", "example.com", "443"},
		{"explicit default port", "http://example.com:80", true, "http", "example.com", "80"},
		{"host case normalized", "http://EXAMPLE.com:7070", true, "http", "example.com", "7070"},
		{"scheme case normalized", "HTTP://example.com:7070", true, "http", "example.com", "7070"},
		{"trailing slash tolerated", "http://example.com:7070/", true, "http", "example.com", "7070"},

		// --- Reject: the attack surface ---
		{"opaque null origin", "null", false, "", "", ""},
		{"empty", "", false, "", "", ""},
		{"whitespace only", "   ", false, "", "", ""},
		{"schemeless", "//evil.com", false, "", "", ""},
		{"bare host", "evil.com", false, "", "", ""},
		{"relative path", "/api/v1", false, "", "", ""},
		{"with path", "http://evil.com/path", false, "", "", ""},
		{"with query", "http://evil.com?a=b", false, "", "", ""},
		{"with fragment", "http://evil.com#frag", false, "", "", ""},
		{"with userinfo", "http://user:pass@evil.com", false, "", "", ""},
		{"userinfo only", "http://u@evil.com", false, "", "", ""},
		{"javascript scheme", "javascript:alert(1)", false, "", "", ""},
		{"data scheme", "data:text/html,<script>", false, "", "", ""},
		{"file scheme", "file:///etc/passwd", false, "", "", ""},
		{"custom scheme", "myapp://evil.com", false, "", "", ""},
		{"multiple origins space separated", "http://a.com http://b.com", false, "", "", ""},
		{"multiple origins comma separated", "http://a.com,http://b.com", false, "", "", ""},
		{"trailing dot host", "http://localhost.:7070", false, "", "", ""},
		{"empty host", "http://", false, "", "", ""},
		{"empty port", "http://example.com:", false, "", "", ""},
		{"non numeric port", "http://example.com:abc", false, "", "", ""},
		{"port out of range", "http://example.com:99999", false, "", "", ""},
		{"negative port", "http://example.com:-1", false, "", "", ""},
		{"embedded newline", "http://evil.com\nHost: x", false, "", "", ""},
		{"embedded space", "http://evil .com", false, "", "", ""},
		{"bare ipv6 unbracketed", "http://::1:7070", false, "", "", ""},
		{"unterminated ipv6 bracket", "http://[::1:7070", false, "", "", ""},
		{"double scheme", "http://http://evil.com", false, "", "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOrigin(tc.raw)
			if ok != tc.wantOK {
				t.Fatalf("parseOrigin(%q) ok = %v, want %v (got %+v)", tc.raw, ok, tc.wantOK, got)
			}
			if !tc.wantOK {
				return
			}
			if got.scheme != tc.wantScheme || got.host != tc.wantHost || got.port != tc.wantPort {
				t.Errorf("parseOrigin(%q) = %s|%s|%s, want %s|%s|%s",
					tc.raw, got.scheme, got.host, got.port,
					tc.wantScheme, tc.wantHost, tc.wantPort)
			}
		})
	}
}

// TestOriginAllowlist_Allows covers the decision table, including the deliberate
// permissive case for an absent Origin header.
func TestOriginAllowlist_Allows(t *testing.T) {
	allow := NewOriginAllowlist("http://127.0.0.1:7070", "http://localhost:7070")

	tests := []struct {
		name   string
		origin string
		want   bool
	}{
		{"exact loopback match", "http://127.0.0.1:7070", true},
		{"exact localhost match", "http://localhost:7070", true},
		{"absent origin (curl/CLI/navigation)", "", true},
		{"whitespace origin treated as absent", "   ", true},

		{"attacker site", "http://evil.com", false},
		{"attacker site matching suffix", "http://evil-127.0.0.1:7070", false},
		{"attacker host ending in ours", "http://127.0.0.1.attacker.com:7070", false},
		{"attacker prefix", "http://127.0.0.1:7070.evil.com", false},
		{"scheme mismatch https on http listener", "https://127.0.0.1:7070", false},
		{"scheme mismatch http on https listener", "http://127.0.0.1:7070", true},
		{"port mismatch", "http://127.0.0.1:9999", false},
		{"port omitted", "http://127.0.0.1", false},
		{"default port equivalence", "http://127.0.0.1:80", false},
		{"opaque null origin", "null", false},
		{"trailing dot localhost", "http://localhost.:7070", false},
		{"ipv6 vs ipv4 mismatch", "http://[::1]:7070", false},
		{"userinfo smuggling", "http://127.0.0.1:7070@evil.com", false},
		{"case-insensitive host ok", "http://LOCALHOST:7070", true},
		{"path appended", "http://127.0.0.1:7070/api", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := allow.Allows(tc.origin); got != tc.want {
				t.Errorf("Allows(%q) = %v, want %v", tc.origin, got, tc.want)
			}
		})
	}
}

// TestOriginAllowlist_EmptyDeniesEverythingWithOrigin verifies fail-closed behavior.
func TestOriginAllowlist_EmptyDeniesEverythingWithOrigin(t *testing.T) {
	var allow *OriginAllowlist
	if allow.Allows("http://127.0.0.1:7070") {
		t.Error("nil allowlist must deny a present Origin")
	}
	if !allow.Allows("") {
		t.Error("nil allowlist must still allow an absent Origin (non-browser clients)")
	}

	empty := NewOriginAllowlist()
	if empty.Allows("http://127.0.0.1:7070") {
		t.Error("empty allowlist must deny every browser origin")
	}

	// A malformed entry must never widen the allowlist.
	mixed := NewOriginAllowlist("http://127.0.0.1:7070", "http://evil.com/path", "null")
	if !mixed.Allows("http://127.0.0.1:7070") {
		t.Error("valid entry should have been added")
	}
	if mixed.Allows("http://evil.com/path") {
		t.Error("malformed entry must be dropped, not added")
	}
	if mixed.Allows("null") {
		t.Error("\"null\" must be dropped, not added")
	}
}

// TestOriginAllowlist_Add covers incremental construction.
func TestOriginAllowlist_Add(t *testing.T) {
	allow := NewOriginAllowlist()
	if !allow.Add("http://127.0.0.1:7070") {
		t.Fatal("Add(valid) = false, want true")
	}
	if allow.Add("http://evil.com/path") {
		t.Error("Add(malformed) = true, want false")
	}
	if !allow.Allows("http://127.0.0.1:7070") {
		t.Error("added origin should be allowed")
	}
}

// TestOriginsEqual_NoSubstringMatching guards the specific bypass class where a naive
// strings.Contains / HasSuffix comparison is used instead of component equality.
func TestOriginsEqual_NoSubstringMatching(t *testing.T) {
	mine, ok := parseOrigin("http://127.0.0.1:7070")
	if !ok {
		t.Fatal("self origin failed to parse")
	}
	for _, evil := range []string{
		"http://127.0.0.1:7070.evil.com",
		"http://evil.com/?x=127.0.0.1:7070",
		"http://evil-127.0.0.1:7070",
		"http://x127.0.0.1:7070",
	} {
		other, ok := parseOrigin(evil)
		if !ok {
			// Unparseable origins are rejected before comparison; that is also safe.
			continue
		}
		if originsEqual(mine, other) {
			t.Errorf("originsEqual matched attacker origin %q", evil)
		}
	}
}

// TestSelfOrigins covers deriving the allowlist from a bound listener address.
func TestSelfOrigins(t *testing.T) {
	tests := []struct {
		name   string
		addr   string
		scheme string
		want   []string
	}{
		{"loopback v4", "127.0.0.1:7070", "http", []string{"http://127.0.0.1:7070", "http://localhost:7070"}},
		{"loopback v6", "[::1]:7070", "http", []string{"http://[::1]:7070", "http://localhost:7070"}},
		{"wildcard yields nothing", "0.0.0.0:7070", "http", nil},
		{"default port elided", "127.0.0.1:80", "http", []string{"http://127.0.0.1"}},
		{"https default port", "127.0.0.1:443", "https", []string{"https://127.0.0.1"}},
		{"non loopback single origin", "10.0.0.5:7070", "http", []string{"http://10.0.0.5:7070"}},
		{"malformed addr", "not-an-address", "http", nil},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SelfOrigins(tc.addr, tc.scheme)
			if len(got) != len(tc.want) {
				t.Fatalf("SelfOrigins(%q,%q) = %v, want %v", tc.addr, tc.scheme, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("SelfOrigins[%d] = %q, want %q", i, got[i], tc.want[i])
				}
			}
			// Every derived origin must round-trip through the parser.
			for _, o := range got {
				if _, ok := parseOrigin(o); !ok {
					t.Errorf("SelfOrigins produced unparseable origin %q", o)
				}
			}
		})
	}
}
