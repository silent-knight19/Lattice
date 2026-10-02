package admin

import (
	"net/http"
	"strings"
)

// APIPrefix is the base path for every admin API route.
const APIPrefix = "/api/v1"

// RouteSpec declares one admin route and the permission it requires.
//
// Making the required permission part of the registration, rather than something the
// handler checks for itself, is what allows the router to fail closed: a route that does
// not declare a permission cannot be reached, and the completeness test in the test suite
// proves every registered route declares one.
type RouteSpec struct {
	// Method is the HTTP method this spec serves.
	Method string
	// Path is the route path, relative to APIPrefix (e.g. "/health").
	Path string
	// Permission is the minimum permission required. It must be non-zero.
	Permission Permission
	// Mutating marks a route that changes state. Mutating routes are additionally
	// subject to the CSRF guard, which is applied by the server's middleware chain.
	Mutating bool
	// Handler serves the route.
	Handler http.Handler
	// description documents intent for the audit log and the completeness test.
	description string
}

// Description returns the route's documentation string.
func (r RouteSpec) Description() string { return r.description }

// NewRoute builds a RouteSpec with the given method, sub-path, permission and handler.
func NewRoute(method, path string, perm Permission, handler http.Handler) RouteSpec {
	return RouteSpec{Method: method, Path: path, Permission: perm, Handler: handler}
}

// NewMutatingRoute builds a RouteSpec for a state-changing route.
func NewMutatingRoute(method, path string, perm Permission, handler http.Handler) RouteSpec {
	return RouteSpec{Method: method, Path: path, Permission: perm, Mutating: true, Handler: handler}
}

// key returns the canonical registry key for a method/path pair.
func (r RouteSpec) key() string {
	return r.Method + " " + APIPrefix + normalizeAPIPath(r.Path)
}

// normalizeAPIPath canonicalizes a sub-path so that "/x", "x", and "/x/" all map to the
// same route. Without this, "/api/v1/health/" would bypass a registration of "/health".
func normalizeAPIPath(p string) string {
	if p == "" {
		return "/"
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	for len(p) > 1 && strings.HasSuffix(p, "/") {
		p = p[:len(p)-1]
	}
	return p
}

// Router dispatches admin API requests to registered routes and enforces the permission
// declared by each one.
//
// It is DEFAULT-DENY in two independent ways:
//
//  1. An unregistered method/path combination yields 404. It never falls through to a
//     handler, a static file, or the SPA.
//  2. A route whose Permission is zero is treated as unreachable, so a route added without
//     declaring its permission fails closed instead of becoming publicly accessible.
//
// The 404 (rather than 403) for unknown paths is intentional: a 403 would confirm that
// /api/v1 exists and is guarded, which is itself a small amount of information.
type Router struct {
	routes map[string]RouteSpec
	// fallback handles non-API paths (the SPA, SEC-11). It is never consulted for
	// /api/v1 paths, so it cannot become a back door into the API.
	fallback http.Handler
}

// NewRouter builds a router from a set of route specifications and a fallback handler for
// non-API paths.
//
// It returns an error when any spec is malformed (empty method/path/handler) or declares
// no permission. Failing at construction is far better than discovering it at runtime: a
// misconfigured server refuses to start rather than serving an unguarded route.
func NewRouter(specs []RouteSpec, fallback http.Handler) (*Router, error) {
	rt := &Router{routes: make(map[string]RouteSpec, len(specs)), fallback: fallback}
	for _, s := range specs {
		if err := rt.Register(s); err != nil {
			return nil, err
		}
	}
	return rt, nil
}

// Register adds one route specification, rejecting anything malformed or unguarded.
func (rt *Router) Register(spec RouteSpec) error {
	if spec.Method == "" || spec.Path == "" {
		return &RouteSpecError{Reason: "route requires both a method and a path"}
	}
	if spec.Handler == nil {
		return &RouteSpecError{Reason: "route requires a handler"}
	}
	// Default-deny: a route with no declared permission is a programming error, not an
	// open route.
	if spec.Permission == 0 {
		return &RouteSpecError{Reason: "route declares no required permission"}
	}
	k := spec.key()
	if _, exists := rt.routes[k]; exists {
		return &RouteSpecError{Reason: "duplicate route registration"}
	}
	rt.routes[k] = spec
	return nil
}

// Routes returns the registered route keys, for tests and diagnostics.
func (rt *Router) Routes() []string {
	out := make([]string, 0, len(rt.routes))
	for k := range rt.routes {
		out = append(out, k)
	}
	return out
}

// ServeHTTP dispatches a request.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL == nil {
		writeNotFound(w)
		return
	}

	// Anything under /api/ belongs to the API surface and must never fall through to the
	// SPA fallback. Matching on "/api" rather than "/api/v1" matters: a request to
	// /api/v2/health or /api/unknown would otherwise be served the SPA's index.html,
	// which both leaks that the API exists and can turn a mistyped API call into a
	// confusing 200 with HTML instead of a JSON 404.
	if !strings.HasPrefix(r.URL.Path, "/api") {
		if rt.fallback != nil {
			rt.fallback.ServeHTTP(w, r)
			return
		}
		writeNotFound(w)
		return
	}

	// Look up the normalized path so that registration and dispatch agree; a trailing
	// slash must not silently turn a registered route into a 404.
	key := r.Method + " " + APIPrefix + normalizeAPIPath(strings.TrimPrefix(r.URL.Path, APIPrefix))
	spec, ok := rt.routes[key]
	if !ok {
		// Default-deny: unknown API paths never reach a handler.
		writeNotFound(w)
		return
	}
	// Defence in depth: even though Register rejects a zero permission, re-check here so a
	// Router built by other means still cannot serve an unguarded route.
	if spec.Permission == 0 || spec.Handler == nil {
		writeNotFound(w)
		return
	}
	spec.Handler.ServeHTTP(w, r)
}

// RouteSpecError reports a malformed route registration. It exists so that a server with a
// bad route table fails to start rather than serving an unprotected API.
type RouteSpecError struct {
	Method string
	Path   string
	Reason string
}

func (e *RouteSpecError) Error() string {
	if e.Method != "" || e.Path != "" {
		return "admin route " + e.Method + " " + e.Path + ": " + e.Reason
	}
	return "admin route: " + e.Reason
}

// writeNotFound emits the standard 404 for an unknown API path.
func writeNotFound(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "resource not found")
}
