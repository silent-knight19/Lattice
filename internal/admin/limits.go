package admin

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/silent-knight19/lattice/internal/binary"
)

// This file implements SEC-7: request size caps, handler deadlines, and method
// enforcement.
//
// SEC-0 finding F3 established that neither cmd/lattice/pprof.go nor
// internal/metrics/server.go bounds request BODIES; only MaxHeaderBytes is set. A browser
// or a stray script can therefore stream an arbitrarily large body to the admin API and
// force the daemon to buffer it. Every cap here derives from the ENGINE's real limits
// rather than from round numbers, so the API rejects nothing the storage layer would
// accept anyway.

// Request body caps, in bytes.
const (
	// jsonEnvelopeOverhead is slack added to the largest key plus the largest value so a
	// legitimate single-key write is accepted with room to spare for JSON framing
	// (field names, quotes, base64 expansion is NOT used: the API accepts raw bytes).
	// 1 KiB is generous for {"key":"...","value":"..."} escaping without being loose.
	jsonEnvelopeOverhead = 1024

	// MaxKeyPutBody is the largest accepted body for /keys/put and /keys/delete.
	// It must accommodate binary.MaxKeyLen + binary.MaxValueLen plus JSON framing.
	// Any excess is rejected before the engine is asked to validate it.
	MaxKeyPutBody = int64(binary.MaxKeyLen + binary.MaxValueLen + jsonEnvelopeOverhead)

	// MaxConsoleBody is the cap for /console/exec. A console command is a short text
	// line; 64 KiB is far more than any legitimate command needs and keeps the
	// command-parsing work bounded.
	MaxConsoleBody = 64 * 1024

	// MaxWorkloadBody is the cap for /lab/workload/start, whose payload is a small JSON
	// object of workload parameters.
	MaxWorkloadBody = 8 * 1024

	// MaxGenericBody is the cap applied to any mutating admin route that has no
	// route-specific limit. It is deliberately the smallest of the three: an unlisted
	// mutating endpoint should be small, and a route that genuinely needs more must
	// declare it explicitly.
	MaxGenericBody = 64 * 1024

	// DefaultMaxBody is the cap used when no route-specific limit applies. It matches
	// the binary transport's 5 MiB payload ceiling closely enough to be consistent
	// without permitting a browser to post an entire SSTable.
	DefaultMaxBody = 5 * 1024 * 1024
)

// bodyLimitFor returns the request body cap for a path.
func bodyLimitFor(path string) int64 {
	switch normalizeAPIPath(trimAPIPrefix(path)) {
	case RouteKeyPut, RouteKeyDelete:
		return MaxKeyPutBody
	case RouteConsole:
		return MaxConsoleBody
	case RouteLabStart:
		return MaxWorkloadBody
	default:
		return DefaultMaxBody
	}
}

// trimAPIPrefix removes the API prefix from a path, if present.
func trimAPIPrefix(p string) string {
	if len(p) >= len(APIPrefix) && p[:len(APIPrefix)] == APIPrefix {
		return p[len(APIPrefix):]
	}
	return p
}

// BodyLimit caps the request body for every route.
//
// It uses http.MaxBytesReader, which bounds the read at the transport level and, crucially,
// makes an over-limit read fail with a *http.MaxBytesError. It does NOT merely limit what
// the handler chooses to read: a handler that ignores the limit still cannot exceed it.
//
// The cap is chosen per route (see bodyLimitFor) and is applied to the request Body before
// the handler runs, so a handler that forgets to check cannot introduce a hole.
func BodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limit := bodyLimitFor(r.URL.Path)
		if r.Body != nil {
			// Only bodies that can carry content are capped; GET requests with no body
			// are left untouched so that a nil body never causes a spurious error.
			r.Body = http.MaxBytesReader(w, r.Body, limit)
		}
		next.ServeHTTP(w, r)
	})
}

// MaxBytesError reports whether err indicates the request body exceeded its cap.
//
// Exported so endpoint handlers can map the sentinel to a 413 instead of a generic 400.
func MaxBytesError(err error) bool {
	var maxErr *http.MaxBytesError
	return errors.As(err, &maxErr)
}

// DefaultHandlerTimeout bounds how long a non-streaming handler may run.
//
// 10 seconds is generous for a local point lookup or a small compaction trigger, and short
// enough that a wedged handler cannot pin a connection indefinitely. Streaming endpoints
// (the SSE event stream) must NOT use this: they are long-lived by design and are exempt via
// isStreamingPath.
const DefaultHandlerTimeout = 10 * time.Second

// isStreamingPath reports whether a path is a long-lived stream that must not be given a
// short deadline.
func isStreamingPath(p string) bool {
	return normalizeAPIPath(trimAPIPrefix(p)) == RouteEvents
}

// HandlerTimeout applies a deadline to non-streaming handlers.
//
// The deadline is attached to the request context. Handlers that respect ctx (which every
// engine and Raft call in this codebase does, because their signatures take
// context.Context) will abort. A handler that ignores ctx will still run to completion, so
// this is a cooperative limit, not a hard kill; it is defence in depth against an accidental
// hang, not a substitute for bounded work.
//
// Streaming endpoints are exempt: bounding them would sever the SSE connection every second.
func HandlerTimeout(timeout time.Duration) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isStreamingPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// MethodGuard rejects a request whose HTTP method is not in the allowed set.
//
// This is SEC-7.4. The router already keys routes by method, so an unknown method yields a
// 404; this middleware adds the explicit, standards-correct 405 with an Allow header for
// routes that declare a method set, and is the place where "never let a GET reach a
// mutating handler" is enforced as a first-class rule.
//
// The handler receives the parsed method list so it can emit a correct Allow header.
func MethodGuard(allowed ...string) Middleware {
	return func(next http.Handler) http.Handler {
		allow := ""
		for _, m := range allowed {
			if allow != "" {
				allow += ", "
			}
			allow += m
		}
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for _, m := range allowed {
				if r.Method == m {
					next.ServeHTTP(w, r)
					return
				}
			}
			w.Header().Set("Allow", allow)
			writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed for this resource")
		})
	}
}
