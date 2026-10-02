package admin

import (
	"net/http"
	"strings"
)

// Middleware is the standard decorator signature used throughout the admin server.
type Middleware func(http.Handler) http.Handler

// Chain applies middlewares so that the first argument is the outermost layer.
//
//	Chain(h, Recoverer(), SecurityHeaders(), OriginGuard(allow)) // h runs last
//
// Order is a security property, not a style choice. See Server.middleware for the
// canonical ordering and the reasoning behind it.
func Chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		if mws[i] != nil {
			h = mws[i](h)
		}
	}
	return h
}

// OriginGuard rejects requests whose Origin header identifies a web origin other than one
// in the allowlist.
//
// This is SEC-1 and the PRIMARY control against the drive-by localhost attack proven in
// docs/user interface/sec0-spike-findings.md. Loopback binding does not stop a foreign web
// page from causing the browser to send requests here; refusing the Origin is the only
// server-side action that prevents delivery.
//
// Design notes:
//
//   - The check is applied to ALL methods, not just mutating ones. A defense that only
//     guards writes still permits cross-origin *reads* to be attempted and, more
//     importantly, invites a future developer to add a mutating route assuming the guard
//     does not apply to it.
//   - Rejection is uniform: 403 with a fixed body. No permitted origins, no list of known
//     origins, and no echo of the supplied Origin are disclosed to the caller.
//   - The response deliberately carries NO Access-Control-* header. Emitting one would
//     re-enable the browser to deliver and expose cross-origin responses.
//
// Because OriginGuard is a pure gate that returns a constant response, the security
// assertions in the test suite are made against the SERVER's behaviour. Per SEC-0 finding
// F12, a client-side test cannot observe whether a mutating request was actually executed,
// because the browser reports a network error either way.
func OriginGuard(allow *OriginAllowlist) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !allow.Allows(r.Header.Get("Origin")) {
				// 403 rather than 401: the request is understood but forbidden.
				// Deliberately terse and constant so the response is not an oracle.
				writeForbidden(w)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// writeForbidden emits the standard 403 response for a request rejected by a security
// gate.
//
// It sets no CORS headers, no cache directives beyond no-store, and a body that reveals
// nothing about the server's configuration. SEC-5 will add the full header set; this
// function is the single place that decides what a rejection looks like.
func writeForbidden(w http.ResponseWriter) {
	// No Access-Control-* headers. See OriginGuard's documentation.
	writeError(w, http.StatusForbidden, "forbidden", "request origin is not permitted")
}

// corsForbiddenResponseHeaders lists headers that must never appear on any admin
// response. Used by the no-CORS invariant test.
var corsForbiddenResponseHeaders = []string{
	"Access-Control-Allow-Origin",
	"Access-Control-Allow-Credentials",
	"Access-Control-Allow-Methods",
	"Access-Control-Allow-Headers",
	"Access-Control-Expose-Headers",
	"Access-Control-Max-Age",
	"Access-Control-Allow-Private-Network",
}

// hasAnyCORSHeader reports whether h carries any CORS response header. Used by the
// invariant test that guards against a future contributor adding permissive CORS.
func hasAnyCORSHeader(h http.Header) bool {
	for _, name := range corsForbiddenResponseHeaders {
		if h.Get(name) != "" {
			return true
		}
	}
	// Also catch headers not in the known list (e.g. Access-Control-Foo).
	for name := range h {
		if strings.HasPrefix(http.CanonicalHeaderKey(name), "Access-Control-") {
			return true
		}
	}
	return false
}
