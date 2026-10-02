package admin

import (
	"net/http"
	"strings"
)

// This file implements SEC-5: security response headers and Content Security Policy.
//
// Why this matters for Lattice specifically:
//
//   - Keys and values are ATTACKER-CONTROLLED BYTES. The console renders them. CSP is the
//     backstop that makes an XSS bug non-exploitable, because 'self' script-src means
//     injected markup cannot execute even if a rendering flaw slips through.
//   - The Lab page has a "crash this node" button. X-Frame-Options: DENY plus
//     frame-ancestors 'none' prevents clickjacking, where an attacker frames the console
//     and tricks an operator into clicking a destructive control.
//   - Every API response carries operational data. no-store keeps it out of shared caches
//     and the browser disk cache.

// ContentSecurityPolicy is the CSP applied to every admin response.
//
// Deliberate omissions, each a security decision rather than an oversight:
//
//   - NO 'unsafe-inline' and NO 'unsafe-eval' anywhere. This forbids inline <script> and
//     eval(). The consequence is recorded: any inline style or inline handler in the SPA
//     build must be moved to bundled CSS or a nonce (see ui-console-tasks.md FE-3/TB-3).
//   - 'self' for script-src means an injected <script> cannot run, even inline.
//   - object-src 'none' blocks plugin content.
//   - base-uri 'none' blocks <base> hijacking, which could otherwise re-point every
//     relative URL in the document.
//   - form-action 'none' blocks form-submission exfiltration: without it, an injected
//     <form> could POST key data to an attacker's server, which CSP script-src would NOT
//     stop.
//   - frame-ancestors 'none' is the modern clickjacking defense; X-Frame-Options: DENY is
//     retained alongside it for older agents.
//
// connect-src 'self' still permits the SSE stream to /api/v1/events, since that is a
// same-origin fetch.
const ContentSecurityPolicy = "default-src 'self'; " +
	"script-src 'self'; " +
	"style-src 'self'; " +
	"img-src 'self' data:; " +
	"connect-src 'self'; " +
	"font-src 'self'; " +
	"object-src 'none'; " +
	"base-uri 'none'; " +
	"form-action 'none'; " +
	"frame-ancestors 'none'"

// PermissionsPolicy disables browser capabilities the console never uses. Quoting is
// required by the specification and is also what makes the directive fail closed.
const PermissionsPolicy = "accelerometer=(), autoplay=(), camera=(), display-capture=(), " +
	"encrypted-media=(), fullscreen=(), geolocation=(), gyroscope=(), magnetometer=(), " +
	"microphone=(), midi=(), payment=(), usb=(), " +
	"clipboard-read=(), clipboard-write=(), interest-cohort=()"

// securityHeaderNames lists every header this middleware sets, used by the golden test to
// assert completeness in both directions.
var securityHeaderNames = []string{
	"Content-Security-Policy",
	"X-Content-Type-Options",
	"X-Frame-Options",
	"Referrer-Policy",
	"Cross-Origin-Opener-Policy",
	"Cross-Origin-Resource-Policy",
	"Permissions-Policy",
}

// SecurityHeaders applies the full SEC-5 header set to every response.
//
// It must sit near the OUTSIDE of the chain so that even a rejection produced by an inner
// guard carries the headers: an error page without CSP is still an XSS sink if it ever
// reflects input.
//
// The middleware sets headers BEFORE calling next, so a handler that forgets to set them
// cannot produce a headerless response. Handlers may still override individual headers
// (e.g. Cache-Control for a cacheable static asset); those overrides are deliberate and
// are not undone here.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", ContentSecurityPolicy)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		// Do not leak the console URL (which can embed a node id or a diagnostic view
		// selection) to third parties through the Referer header.
		h.Set("Referrer-Policy", "no-referrer")
		// Isolate the console from cross-origin window relationships, preventing
		// reverse-tabnabbing style attacks against an operator mid-session.
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Cross-Origin-Resource-Policy", "same-origin")
		h.Set("Permissions-Policy", PermissionsPolicy)

		next.ServeHTTP(w, r)
	})
}

// NoStoreAPI forces Cache-Control: no-store on API responses while leaving static assets
// cacheable.
//
// It is separate from SecurityHeaders because caching is a property of the RESOURCE, not a
// blanket security property: the SPA's hashed bundle should be cacheable, whereas every
// /api/v1 response is live operational state.
//
// If a handler has already set a Cache-Control value, it is left alone: the admin API may
// deliberately choose a stricter policy in future, and silently overwriting a handler's
// decision would make that impossible to reason about.
func NoStoreAPI(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) && w.Header().Get("Cache-Control") == "" {
			w.Header().Set("Cache-Control", "no-store")
		}
		next.ServeHTTP(w, r)
	})
}

// isAPIPath reports whether a path belongs to the JSON API surface.
func isAPIPath(p string) bool {
	return p == "/api" || strings.HasPrefix(p, "/api/")
}

// contentTypeByExtension maps the file extensions the SPA build emits to an explicit MIME
// type.
//
// SEC-5.2 requires that asset serving never rely on content sniffing. An explicit type,
// combined with nosniff, means a malicious or mislabelled file cannot be reinterpreted by
// the browser as script. The default is a non-executable type for the same reason.
//
// This is the mapping only; the embed.FS routing and SPA fallback belong to SEC-11.
func contentTypeByExtension(path string) string {
	// Only the final extension matters, and the comparison is case-insensitive because
	// build tooling and filesystems disagree about case.
	lower := strings.ToLower(path)
	if i := strings.LastIndex(lower, "."); i >= 0 {
		switch lower[i:] {
		case ".html", ".htm":
			return "text/html; charset=utf-8"
		case ".js", ".mjs":
			// text/javascript is the modern, standards-recommended type; the older
			// application/javascript is accepted by every browser and avoided here.
			return "text/javascript; charset=utf-8"
		case ".css":
			return "text/css; charset=utf-8"
		case ".json":
			return "application/json"
		case ".map":
			return "application/json"
		case ".svg":
			// SVG can contain script; serving it as image/svg+xml plus nosniff and the
			// CSP is what keeps it inert.
			return "image/svg+xml"
		case ".png":
			return "image/png"
		case ".jpg", ".jpeg":
			return "image/jpeg"
		case ".gif":
			return "image/gif"
		case ".webp":
			return "image/webp"
		case ".ico":
			return "image/x-icon"
		case ".woff":
			return "font/woff"
		case ".woff2":
			return "font/woff2"
		case ".ttf":
			return "font/ttf"
		case ".txt":
			return "text/plain; charset=utf-8"
		case ".wasm":
			return "application/wasm"
		}
	}
	// Unknown or extension-less files are served as an opaque download rather than
	// something a browser might execute.
	return "application/octet-stream"
}
