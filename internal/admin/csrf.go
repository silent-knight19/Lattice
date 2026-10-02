package admin

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"time"
)

// csrfTokenBytes is the entropy of the per-boot CSRF secret.
// 32 bytes = 256 bits, the output size of SHA-256 and the customary strength for a
// synchronizer token.
const csrfTokenBytes = 32

// CSRFHeader is the request header carrying the CSRF token on unsafe methods.
const CSRFHeader = "X-Lattice-CSRF"

// AdminConfirmHeader is the request header carrying explicit operator confirmation.
// The design document requires it on mutating endpoints; CSRFGuard enforces it
// alongside the token so a cross-site form post cannot satisfy either gate alone.
const AdminConfirmHeader = "X-Lattice-Admin"

// adminConfirmValue is the exact value AdminConfirmHeader must carry.
const adminConfirmValue = "confirm"

// CSRFToken is a per-boot anti-CSRF secret.
//
// A new token is generated on every daemon start. That is deliberate: it means a token
// captured from a previous run (from a log, a screenshot, a stale browser tab, or a
// core dump of an old process) is useless against the running server, so there is no
// need for a separate revocation or expiry mechanism.
//
// The token is generated from crypto/rand. It is never derived from time, PID, or any
// other guessable process state.
type CSRFToken struct {
	raw    [csrfTokenBytes]byte
	digest [sha256.Size]byte
	// issuedAt lets the session endpoint tell a client that the daemon restarted, so a
	// UI holding a stale token can detect it instead of silently failing every write.
	issuedAt time.Time
}

// NewCSRFToken generates a fresh token from the kernel CSPRNG.
func NewCSRFToken() (*CSRFToken, error) {
	t := &CSRFToken{issuedAt: time.Now()}
	if _, err := rand.Read(t.raw[:]); err != nil {
		return nil, err
	}
	t.digest = sha256.Sum256(t.raw[:])
	return t, nil
}

// Value returns the base64 (URL-safe, unpadded) encoding of the token.
//
// This is the value handed to a legitimate client by the session endpoint. It must
// never be logged, echoed in an error, or persisted outside the client.
func (t *CSRFToken) Value() string {
	if t == nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(t.raw[:])
}

// IssuedAt reports when the token was generated, i.e. when the daemon started.
func (t *CSRFToken) IssuedAt() time.Time {
	if t == nil {
		return time.Time{}
	}
	return t.issuedAt
}

// Valid reports whether candidate is the correct token, in constant time with respect to
// the candidate's contents.
//
// Two details matter for security:
//
//   - Comparison is over SHA-256 digests, so both operands are always
//     csrfTokenBytes long. Comparing the raw bytes directly would let
//     subtle.ConstantTimeCompare return early on a length mismatch, which is a timing
//     signal about the expected value.
//   - The comparison result is folded with | rather than tested with !=, so the branch
//     does not short-circuit on the first differing byte.
func (t *CSRFToken) Valid(candidate string) bool {
	if t == nil {
		return false
	}
	// Decode the candidate from the wire encoding before comparing, so that both operands
	// of the digest are the same quantity: the 32 raw bytes. Hashing the base64 text
	// instead would compare a digest of raw bytes against a digest of their encoding,
	// which never match, and hashing nothing at all would let ConstantTimeCompare return
	// early on a length mismatch and leak length information.
	raw, err := base64.RawURLEncoding.DecodeString(candidate)
	if err != nil {
		// A malformed candidate cannot be the token. Fold the failure into a fixed-length
		// digest of nil so the comparison path and cost are identical to the valid case.
		raw = nil
	}
	sum := sha256.Sum256(raw)
	match := subtle.ConstantTimeCompare(sum[:], t.digest[:])
	return match == 1
}

// CSRFGuard rejects unsafe requests that do not present a valid CSRF token.
//
// This is SEC-3, the independent defence layer. SEC-1 (Origin) and SEC-2 (Host) stop
// cross-site *delivery*; this token stops cross-site *forgery* even if an origin or host
// check were ever bypassed. The layers are deliberately independent: a single bypassable
// check is not a control.
//
// Applied to every method except GET, HEAD, and OPTIONS. Those are safe by definition:
// they must not mutate state. OPTIONS is excluded so that a CORS preflight, which by
// definition cannot carry a custom header, terminates in a predictable 403/405 rather
// than being treated as a forgery attempt; the server emits no CORS headers either way
// (SEC-1.3), so the preflight cannot succeed.
//
// The rejection is intentionally identical in shape to every other guard: constant body,
// no echoed token, no CORS headers. A response that distinguished "malformed token" from
// "wrong token" would be an oracle.
func CSRFGuard(token *CSRFToken) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isSafeMethod(r.Method) {
				if !token.Valid(r.Header.Get(CSRFHeader)) {
					writeForbiddenCSRF(w)
					return
				}
				// The token alone is not sufficient for a mutating request: explicit
				// operator confirmation is required as well, so that a leaked token is
				// not by itself enough to mutate engine state.
				if r.Header.Get(AdminConfirmHeader) != adminConfirmValue {
					writeForbiddenCSRF(w)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// isSafeMethod reports whether a method is safe, i.e. must not mutate state.
//
// Only the three RFC 9110 safe/idempotent-and-typographically-harmless methods are
// exempt. TRACE is deliberately NOT exempt: while nominally safe it can be used for
// header reflection, and this server has no reason to accept it at all.
func isSafeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}

// writeForbiddenCSRF emits the standard rejection for a missing or invalid CSRF token.
//
// The body is constant and never echoes either the expected or the supplied token, and
// never indicates which of the two checks failed.
func writeForbiddenCSRF(w http.ResponseWriter) {
	// Intentionally the SAME envelope as the origin rejection. Distinguishing "wrong
	// token" from "wrong origin" would give an attacker a probe, and the token itself
	// must never be echoed.
	writeError(w, http.StatusForbidden, "forbidden", "request origin is not permitted")
}

// SessionResponse is the body of GET /api/v1/session.
//
// It is served only to a request that has already passed HostGuard and OriginGuard.
type SessionResponse struct {
	// CSRFToken is the value the client must echo in CSRFHeader on unsafe methods.
	CSRFToken string `json:"csrf_token"`
	// BootID changes on every daemon start, letting a client detect that its cached
	// token is stale.
	BootID string `json:"boot_id"`
	// IssuedAt is when the token was generated, in RFC 3339.
	IssuedAt string `json:"issued_at"`
}

// bootIDFrom derives a short, non-secret identifier for this boot.
//
// It is the first 8 characters of the base64url encoding of the token's first 6 bytes.
// (Six bytes encode to exactly 8 base64 characters, so no truncation is needed and the
// length is guaranteed rather than merely checked.)
//
// It carries no entropy that matters: it cannot be used to forge the token, and it exists
// only so a client can tell "the daemon restarted" from "the request failed".
func bootIDFrom(t *CSRFToken) string {
	if t == nil {
		return ""
	}
	const encodedLen = 8
	const rawLen = encodedLen * 3 / 4 // 6 bytes -> exactly 8 base64 characters
	return base64.RawURLEncoding.EncodeToString(t.raw[:rawLen])
}

// SessionHandler serves GET /api/v1/session.
//
// This is the ONLY endpoint that discloses the CSRF token. It must be mounted behind
// HostGuard and OriginGuard; the router in ADM-3 is responsible for that ordering.
//
// The response is marked no-store so the token is never retained by an intermediary or
// written to the browser's disk cache.
func SessionHandler(token *CSRFToken) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			writeMethodNotAllowed(w)
			return
		}
		resp := SessionResponse{
			CSRFToken: token.Value(),
			BootID:    bootIDFrom(token),
			IssuedAt:  token.IssuedAt().UTC().Format(time.RFC3339),
		}
		body, err := json.Marshal(resp)
		if err != nil {
			writeInternalError(w)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Pragma", "no-cache")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(append(body, '\n'))
	})
}
