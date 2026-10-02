// Package admin implements the Lattice Console administrative HTTP API.
//
// This file implements SEC-1: cross-origin request rejection.
//
// Security rationale (established empirically in docs/user interface/sec0-spike-findings.md):
// a loopback bind does NOT prevent an unrelated web page from causing the browser to send
// requests to the admin API. A "simple" cross-origin request (no custom headers, a
// text/plain body, or a form POST) is delivered without any CORS preflight and WITHOUT the
// server's cooperation. The browser merely withholds the response from the attacker's
// script. Consequently, absence of Access-Control-* headers stops cross-origin *reads* but
// not cross-origin *effects*.
//
// The only server-side control that stops delivery is inspecting the Origin header and
// rejecting anything that is not our own origin. This is the primary defense against the
// drive-by localhost attack.
package admin

import (
	"errors"
	"net"
	"net/url"
	"strings"
)

// errInvalidAuthority reports a malformed URL authority in an Origin header value.
// It is intentionally unexported: every rejection path collapses to the same
// client-visible outcome, so callers branch on the boolean from parseOrigin rather
// than inspecting a cause.
var errInvalidAuthority = errors.New("invalid origin authority")

// canonicalOrigin is a parsed, normalized scheme://host:port triple suitable for
// exact-match comparison against a browser-supplied Origin header.
//
// It deliberately stores the components separately rather than a string so that
// comparison can never be subverted by textual tricks (embedded credentials, trailing
// dots, userinfo, or default-port elision).
type canonicalOrigin struct {
	scheme string
	host   string // lowercased, no brackets
	port   string // explicit port; the default for the scheme is materialized
}

// parseOrigin parses a browser-supplied Origin header value and reduces it to its
// scheme, host and port.
//
// It returns ok=false for every input that is not a well-formed, absolute, http(s) origin.
// In particular it rejects:
//
//   - "null" (opaque origins: sandboxed iframes, some redirects, file:// documents)
//   - relative or schemeless values ("//host", "/path")
//   - anything carrying a path, query, or fragment, which a real Origin never has
//   - userinfo (user:pass@host), which no legitimate browser Origin contains
//   - non-http(s) schemes (javascript:, data:, file:, custom app schemes)
//
// Errors are reported as ok=false rather than as an error value because the caller has a
// single action for all rejection cases: refuse the request.
func parseOrigin(raw string) (canonicalOrigin, bool) {
	// An Origin header is a single serialized origin. Multiple origins (as sent by some
	// non-browser clients) are ambiguous and must never be partially matched.
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "null" {
		return canonicalOrigin{}, false
	}
	if raw != strings.ToLower(raw) && !strings.Contains(raw, "//") {
		// Case-insensitive here would be wrong for paths but there are none; keep strict.
		return canonicalOrigin{}, false
	}
	if strings.ContainsAny(raw, " \t\r\n") {
		return canonicalOrigin{}, false
	}

	u, err := url.Parse(raw)
	if err != nil {
		return canonicalOrigin{}, false
	}

	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return canonicalOrigin{}, false
	}
	// A real Origin is scheme://host[:port] and nothing else. url.Parse is permissive, so
	// reject anything carrying extra structure rather than silently ignoring it.
	if u.Opaque != "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return canonicalOrigin{}, false
	}
	if u.Path != "" && u.Path != "/" {
		return canonicalOrigin{}, false
	}
	if u.Host == "" {
		return canonicalOrigin{}, false
	}

	host, port, err := splitHostPortStrict(u.Host, scheme)
	if err != nil {
		return canonicalOrigin{}, false
	}
	return canonicalOrigin{scheme: scheme, host: host, port: port}, true
}

// splitHostPortStrict splits an authority into a lowercased host and an explicit port,
// materializing the scheme's default port when the authority omits it.
//
// net.SplitHostPort is unsuitable here: it returns an error for a bare host ("localhost")
// and does not know about default ports. Browser Origins legitimately omit the default
// port (http://localhost for http on 80), so the default must be filled in before an
// exact comparison can work.
func splitHostPortStrict(authority, scheme string) (string, string, error) {
	defaultPort := "80"
	if scheme == "https" {
		defaultPort = "443"
	}

	host := authority
	port := ""

	// Bracketed IPv6 literal, optionally with a port: [::1] or [::1]:7070
	if strings.HasPrefix(authority, "[") {
		end := strings.LastIndex(authority, "]")
		if end < 0 {
			return "", "", errInvalidAuthority
		}
		host = authority[1:end]
		rest := authority[end+1:]
		switch {
		case rest == "":
			port = defaultPort
		case strings.HasPrefix(rest, ":"):
			port = rest[1:]
		default:
			return "", "", errInvalidAuthority
		}
	} else {
		// Bare IPv6 without brackets is ambiguous (contains colons) and is not a valid
		// URL authority; url.Parse would have mangled it.
		if strings.Count(authority, ":") > 1 {
			return "", "", errInvalidAuthority
		}
		if idx := strings.LastIndex(authority, ":"); idx >= 0 {
			host = authority[:idx]
			port = authority[idx+1:]
		} else {
			port = defaultPort
		}
	}

	if host == "" {
		return "", "", errInvalidAuthority
	}
	// Reject a trailing dot ("localhost.") which is a distinct DNS name that could be
	// registered to resolve elsewhere; normalize nothing silently.
	if strings.HasSuffix(host, ".") {
		return "", "", errInvalidAuthority
	}
	// Port must be numeric and in range.
	if port == "" {
		return "", "", errInvalidAuthority
	}
	n := 0
	for i := 0; i < len(port); i++ {
		if port[i] < '0' || port[i] > '9' {
			return "", "", errInvalidAuthority
		}
		n = n*10 + int(port[i]-'0')
		if n > 65535 {
			return "", "", errInvalidAuthority
		}
	}
	return strings.ToLower(host), port, nil
}

// originsEqual reports whether two canonical origins are identical.
//
// This compares the three normalized components individually. It never performs substring,
// suffix, or prefix matching, which is what makes attacker hostnames such as
// "evil-127.0.0.1.attacker.com" or "127.0.0.1.attacker.com" fail closed.
func originsEqual(a, b canonicalOrigin) bool {
	return a.scheme == b.scheme && a.host == b.host && a.port == b.port
}

// OriginAllowlist is an immutable set of origins permitted to call the admin API.
//
// The zero value is not usable; construct one with NewOriginAllowlist.
type OriginAllowlist struct {
	allowed map[canonicalOrigin]struct{}
}

// NewOriginAllowlist builds an allowlist from origin strings, normalizing each one.
//
// An entry that fails to parse is skipped rather than aborting construction: a malformed
// entry must never widen the allowlist, and silently dropping it keeps the server
// startable while defaulting to "deny". Callers that need to know about bad entries should
// validate configuration separately.
func NewOriginAllowlist(origins ...string) *OriginAllowlist {
	m := make(map[canonicalOrigin]struct{}, len(origins))
	for _, raw := range origins {
		if o, ok := parseOrigin(raw); ok {
			m[o] = struct{}{}
		}
	}
	return &OriginAllowlist{allowed: m}
}

// Add includes an additional origin. It reports false when the entry is malformed and was
// therefore not added.
func (a *OriginAllowlist) Add(raw string) bool {
	if a == nil {
		return false
	}
	o, ok := parseOrigin(raw)
	if !ok {
		return false
	}
	if a.allowed == nil {
		a.allowed = make(map[canonicalOrigin]struct{})
	}
	a.allowed[o] = struct{}{}
	return true
}

// Allows reports whether the supplied Origin header value is permitted.
//
// An empty Origin is treated as ALLOWED. That is deliberate and is the one permissive case:
// browsers omit Origin on same-origin GET/HEAD navigations and subresource loads, and
// non-browser clients (curl, the Lattice CLI, health probes) never send one at all.
// Rejecting absent Origin would break every legitimate non-browser caller while adding no
// security, because an attacker cannot suppress the Origin header on a request it causes
// a browser to make cross-origin.
//
// The "null" opaque origin is NOT allowed: it is rejected by parseOrigin.
func (a *OriginAllowlist) Allows(originHeader string) bool {
	if strings.TrimSpace(originHeader) == "" {
		return true
	}
	if a == nil || len(a.allowed) == 0 {
		// Empty allowlist means no browser origin may call us. Non-browser callers are
		// still fine because they send no Origin.
		return false
	}
	o, ok := parseOrigin(originHeader)
	if !ok {
		return false
	}
	_, permitted := a.allowed[o]
	return permitted
}

// SelfOrigins derives the set of origins a browser could legitimately use to reach a server
// bound to addr.
//
// addr is the address the listener is bound to, not the address a user typed. For a loopback
// bind this yields the loopback names plus localhost.
//
// For a wildcard bind (0.0.0.0 or ::) it returns nil: the concrete address is unknown at
// configuration time and no single origin is legitimate. Callers must then supply an
// explicit allowlist; they must NOT synthesize a wildcard origin, which is unreachable as
// an origin and misleading as configuration.
//
// scheme is "http" or "https" and selects which default port is assumed when the bound
// port is a scheme default.
func SelfOrigins(addr, scheme string) []string {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil
	}
	host = strings.ToLower(strings.Trim(host, "[]"))
	if host == "" {
		return nil
	}

	scheme = strings.ToLower(scheme)
	if scheme != "http" && scheme != "https" {
		scheme = "http"
	}

	// A wildcard bind has no single origin a browser could use. Returning
	// "http://0.0.0.0:7070" would be actively wrong: it is not a reachable origin, and an
	// operator who pasted it into configuration would believe remote access was
	// allowlisted when in fact no browser origin is (or should be) permitted. Fail closed
	// and require an explicit allowlist instead.
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return nil
	}

	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		// Omit a default port so the serialized origin matches what a browser sends.
		return []string{scheme + "://" + host}
	}

	var origins []string
	origins = append(origins, scheme+"://"+net.JoinHostPort(host, port))

	// A loopback listener is also reachable as "localhost", which is a distinct origin
	// string and must be allowed explicitly.
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		origins = append(origins, scheme+"://"+net.JoinHostPort("localhost", port))
	}
	return origins
}
