package admin

import (
	"net"
	"net/http"
	"strings"
)

// HostAllowlist is an immutable set of Host header values permitted to reach the admin API.
//
// It exists to defeat DNS rebinding. In that attack the attacker controls a domain
// (attacker.example) whose DNS record points at 127.0.0.1. The browser connects to the
// loopback admin server believing it is talking to attacker.example, and sends
// "Host: attacker.example". Nothing about that request looks cross-origin in the usual
// sense, so the Origin allowlist alone cannot stop it: the Origin would also be the
// attacker's own origin, which is entirely self-consistent.
//
// Binding to loopback does not help either, because the connection genuinely goes to
// loopback. Pinning the Host header to the address the server is actually reachable at
// is what closes the hole: a rebound request carries a Host that is not ours and is
// rejected before it reaches any handler.
//
// The zero value is not usable; construct one with NewHostAllowlist.
type HostAllowlist struct {
	allowed map[canonicalHost]struct{}
}

// canonicalHost is a normalized host:port pair.
//
// Unlike canonicalOrigin there is no scheme, because Host carries no scheme. The port is
// still materialized using the serving scheme's default so that a browser sending
// "Host: localhost" (port 80 elided) matches a listener on port 80.
type canonicalHost struct {
	host string // lowercased, IPv6 without brackets
	port string
}

// parseHostHeader normalizes a Host header value into a canonicalHost.
//
// It rejects, failing closed:
//   - empty values and values containing whitespace or control characters
//   - userinfo ("user@host"), which no legitimate Host contains
//   - trailing-dot hosts ("localhost."), a distinct registerable DNS name
//   - non-numeric or out-of-range ports
//   - IPv6 literals that are not correctly bracketed
//
// Reusing splitHostPortStrict keeps this parser behaviourally identical to the Origin
// parser, so the two cannot drift apart on edge cases like default-port elision.
func parseHostHeader(raw, scheme string) (canonicalHost, bool) {
	if raw == "" || len(raw) > 512 {
		return canonicalHost{}, false
	}
	// A Host header is a single authority. Embedded CR/LF would be a request-smuggling
	// vector, and Go's server rejects most of these before we see them, but a defense that
	// depends on an upstream guarantee is not a defense.
	if strings.ContainsAny(raw, " \t\r\n/\\?#@") {
		return canonicalHost{}, false
	}
	host, port, err := splitHostPortStrict(raw, scheme)
	if err != nil {
		return canonicalHost{}, false
	}
	return canonicalHost{host: host, port: port}, true
}

// NewHostAllowlist builds an allowlist from Host header values, normalizing each one.
// Malformed entries are skipped so construction cannot fail open.
func NewHostAllowlist(hosts ...string) *HostAllowlist {
	return newHostAllowlist("http", hosts...)
}

// NewHostAllowlistScheme is NewHostAllowlist with an explicit serving scheme, used to
// resolve default-port elision correctly for an HTTPS listener.
func NewHostAllowlistScheme(scheme string, hosts ...string) *HostAllowlist {
	return newHostAllowlist(scheme, hosts...)
}

func newHostAllowlist(scheme string, hosts ...string) *HostAllowlist {
	m := make(map[canonicalHost]struct{}, len(hosts))
	for _, raw := range hosts {
		if h, ok := parseHostHeader(raw, scheme); ok {
			m[h] = struct{}{}
		}
	}
	return &HostAllowlist{allowed: m}
}

// Allows reports whether the supplied Host header value is permitted.
//
// An empty Host is denied. Unlike Origin, there is no legitimate empty-Host case: every
// HTTP/1.1 request carries one, and HTTP/1.0 requests are not something the console needs
// to accommodate. Failing closed here is the point of the guard.
func (h *HostAllowlist) Allows(hostHeader, scheme string) bool {
	if h == nil || len(h.allowed) == 0 {
		return false
	}
	ch, ok := parseHostHeader(hostHeader, scheme)
	if !ok {
		return false
	}
	_, permitted := h.allowed[ch]
	return permitted
}

// SelfHosts derives the Host values a browser could legitimately send to a server bound to
// addr.
//
// The set is derived from the address the listener is bound to, never from user-supplied
// configuration, so an operator cannot accidentally widen it.
//
// For a loopback bind both the loopback IP and "localhost" are included, since a browser
// may use either and they are different strings. For a wildcard bind (0.0.0.0 or ::) no
// Host value can be derived: the concrete address is unknown and a wildcard is not a
// legitimate Host. Callers must then supply an explicit allowlist.
func SelfHosts(addr, scheme string) []string {
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

	// Fail closed for wildcard binds, mirroring SelfOrigins.
	if ip := net.ParseIP(host); ip != nil && ip.IsUnspecified() {
		return nil
	}

	if port == "" || (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		// Default port: the Host may legitimately arrive with or without it.
		return []string{host}
	}

	hosts := []string{net.JoinHostPort(host, port)}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		hosts = append(hosts, net.JoinHostPort("localhost", port))
	}
	return hosts
}

// HostGuard rejects requests whose Host header does not match the allowlist.
//
// This is SEC-2 and the control that defeats DNS rebinding, the gap SEC-1 explicitly
// documents it cannot close.
//
// Design notes:
//   - A rejected request is answered with 400 Bad Request, not 403. An unrecognized Host
//     means the client addressed a server that is not us; that is a malformed request
//     rather than an authorization failure, and the distinction matters when reading logs.
//   - The response body is constant and the guard runs before routing, so a rebound
//     request cannot reach any handler, mutate anything, or be routed.
//   - No CORS headers are emitted, consistent with SEC-1.
func HostGuard(allow *HostAllowlist, scheme string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allow.Allows(r.Host, scheme) {
				writeBadHost(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeBadHost emits the standard 400 response for a request with an unrecognized Host.
func writeBadHost(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusBadRequest)
	_, _ = w.Write([]byte(`{"error":{"code":"bad_host","message":"request host is not permitted"}}` + "\n"))
}
