package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
)

// RequestIDHeader is the response header carrying the per-request correlation identifier.
const RequestIDHeader = "X-Request-Id"

// requestIDKey is the context key under which the request id is stored.
type requestIDKey struct{}

// NewRequestID returns a fresh 128-bit random request identifier.
//
// It is generated from crypto/rand rather than being sequential or derived from the clock,
// for two reasons:
//
//   - It is echoed to the client and appears in logs. A predictable counter would let a
//     caller guess another request's id and could be used to correlate or forge log lines.
//   - It must carry no information about the request itself.
func NewRequestID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand failing is unrecoverable for an id source. Rather than fall back to a
		// predictable value, return a fixed sentinel: correctness of the ID scheme matters
		// less than never emitting a guessable identifier.
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

// WithRequestID returns a context carrying id.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, id)
}

// RequestIDFrom returns the request id stored in ctx, or "" when absent.
func RequestIDFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	if v, ok := ctx.Value(requestIDKey{}).(string); ok {
		return v
	}
	return ""
}

// RequestIDOf returns the request id for an in-flight request.
func RequestIDOf(r *http.Request) string {
	if r == nil {
		return ""
	}
	return RequestIDFrom(r.Context())
}

// RequestID assigns every request an id, places it in the context, and echoes it in the
// response header.
//
// It exists so that an operator can quote a single opaque token from a browser error and find
// the exact server-side detail in the logs, without that detail ever crossing the wire
// (SEC-9.3/9.4).
//
// An inbound X-Request-Id is IGNORED rather than trusted: it is client-supplied, and echoing
// it back unvalidated would let a caller inject chosen content into log lines.
func RequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := NewRequestID()
		w.Header().Set(RequestIDHeader, id)
		next.ServeHTTP(w, r.WithContext(WithRequestID(r.Context(), id)))
	})
}
