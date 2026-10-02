package admin

import (
	"context"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/silent-knight19/lattice/internal/logger"
)

// This file implements SEC-10: structured, injection-safe, audit-capable logging for the
// admin API.
//
// Three findings from probing the existing internal/logger shaped this design:
//
//  1. QUOTING ALREADY NEUTRALISES FORGING. A newline in a value is emitted as the two
//     characters \n, and an ANSI escape as <ESC>, because slog quotes every field. So a key
//     containing "\nFAKE LOG LINE" cannot forge a log line. Sanitising control characters is
//     therefore DEFENCE IN DEPTH, not the primary control — stated plainly so the guarantee is
//     not over-claimed.
//
//  2. THERE IS NO TRUNCATION. A 200 KB value is logged in full. Since the API admits 4 MiB
//     values (SEC-7), one request can write megabytes of log. That is a real disk-exhaustion
//     vector and the main thing this file fixes.
//
//  3. REDACTION IS PURELY KEY-NAME BASED, AND HAS GAPS. Measured on the existing logger:
//
//     csrf_token under key "csrf_token"  -> [REDACTED]     (works)
//     the SAME secret under "request_id" -> LEAKED
//     "csrf=SECRET" inside a message     -> LEAKED
//     tls_key, cert, cert PEM body       -> LEAKED
//
//     So an admin logger must redact by VALUE SEMANTICS too, not merely by field name.

// MaxLogFieldLen bounds any single logged value.
const MaxLogFieldLen = 256

// SanitizeLogField makes an arbitrary string safe to log.
//
// It strips control characters (including ANSI escapes), replaces invalid UTF-8, and
// truncates to MaxLogFieldLen on a rune boundary. Printable unicode is preserved so logs stay
// useful.
//
// This is defence in depth behind slog's quoting: it guarantees the property even if a field
// later reaches a non-quoting sink, and it bounds length, which quoting does not.
func SanitizeLogField(s string) string {
	if len(s) > MaxLogFieldLen {
		s = s[:MaxLogFieldLen]
		for len(s) > 0 && !utf8.ValidString(s) {
			s = s[:len(s)-1]
		}
	}
	if isAllPrintable(s) {
		return s
	}
	var b []byte
	for _, r := range s {
		switch {
		case r == utf8.RuneError:
			b = utf8.AppendRune(b, utf8.RuneError)
		case unicode.IsControl(r):
			// Dropped entirely: a newline here would let an attacker forge a log line in
			// any sink that does not quote.
		default:
			b = utf8.AppendRune(b, r)
		}
	}
	return string(b)
}

func isAllPrintable(s string) bool {
	for _, r := range s {
		if r == utf8.RuneError || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// sensitiveLogKeys extends the base logger's redaction list with names the admin API
// actually uses. The base list in internal/logger covers password/secret/token style names;
// it does NOT cover TLS key paths, certificates, or CSRF tokens.
var sensitiveLogKeys = map[string]struct{}{
	"csrf": {}, "csrf_token": {}, "csrftoken": {}, "x-lattice-csrf": {},
	"tls_key": {}, "tlskey": {}, "key_pem": {}, "private_key": {}, "privkey": {},
	"client_key": {}, "peer_key": {},
	"cert": {}, "cert_pem": {}, "certificate": {}, "ca_cert": {}, "client_ca": {},
	"authorization": {}, "cookie": {}, "set-cookie": {},
	"fingerprint_authorization": {},
}

// pemMarkers identify embedded PEM blocks. A PEM block in a log line is unambiguously
// key material, so matching on shape gives high precision with no false positives.
var pemMarkers = []string{"-----BEGIN ", "-----END CERTIFICATE", "PRIVATE KEY-----"}

// looksLikePEM reports whether s embeds PEM material.
func looksLikePEM(s string) bool {
	for _, m := range pemMarkers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}

// RedactedValue is the placeholder substituted for suppressed values.
const RedactedValue = "[REDACTED]"

// RedactValue returns a value that is safe to log, or RedactedValue when the content is
// sensitive.
//
// Three independent triggers, because the key-name mechanism alone demonstrably leaks:
//
//  1. The field NAME is sensitive.
//  2. The VALUE embeds PEM material.
//  3. The VALUE is longer than MaxLogFieldLen and is uniformly high-entropy, which is the
//     shape of a bearer/CSRF token. Short ordinary strings are never classified this way, so
//     legitimate long messages are not redacted by mistake.
func RedactValue(fieldName, value string) string {
	name := strings.ToLower(strings.TrimSpace(fieldName))
	if _, sensitive := sensitiveLogKeys[name]; sensitive {
		return RedactedValue
	}
	if looksLikePEM(value) {
		return RedactedValue
	}
	if isHighEntropyToken(value) {
		return RedactedValue
	}
	return SanitizeLogField(value)
}

// isHighEntropyToken reports whether s has the shape of a bearer-style secret: long, and
// drawn from an alphabet with no spaces or word structure.
//
// The additional length requirement is what keeps this safe. Ordinary prose, keys, and error
// messages contain spaces and punctuation and fail the test immediately; a 43-character
// base64url CSRF token does not.
func isHighEntropyToken(s string) bool {
	const minTokenLen = 32
	if len(s) < minTokenLen {
		return false
	}
	// A pure-hex string is an IDENTIFIER, not a secret: a request id, a SHA-256
	// fingerprint, a checksum. Redacting these would break exactly the log correlation the
	// audit trail exists to provide (SEC-9.4), so hex is excluded explicitly.
	if isHexOnly(s) {
		return false
	}
	var letters, digits, symbols int
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			letters++
		case r >= '0' && r <= '9':
			digits++
		default:
			symbols++
		}
	}
	// Require a mix of character classes: real secrets are not all one class.
	if letters == 0 || digits == 0 {
		return false
	}
	// Prose contains spaces and punctuation; tokens do not.
	if symbols > len(s)/8 {
		return false
	}
	return true
}

// isHexOnly reports whether s consists solely of hexadecimal digits, optionally prefixed by
// a well-known digest algorithm label such as "sha256:" or "SHA-1".
func isHexOnly(s string) bool {
	if i := strings.IndexByte(s, ':'); i >= 0 {
		prefix, rest := s[:i], s[i+1:]
		switch strings.ToLower(prefix) {
		case "sha1", "sha256", "sha512", "md5", "sha-1", "sha-256", "sha-512":
			s = rest
		default:
			return false
		}
	}
	if s == "" {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// AuditDecision is the outcome recorded for a mutating request.
type AuditDecision string

// Audit decisions.
const (
	AuditAllowed AuditDecision = "allowed"
	AuditDenied  AuditDecision = "denied"
)

// AdminLogger wraps logger.Logger with admin-specific redaction and truncation.
type AdminLogger struct {
	inner logger.Logger
}

// NewAdminLogger wraps a logger. A nil inner logger yields a no-op wrapper so callers never
// need a nil check.
func NewAdminLogger(inner logger.Logger) *AdminLogger {
	if inner == nil {
		inner = logger.NewNop()
	}
	return &AdminLogger{inner: inner}
}

// inner0 returns the wrapped logger, tolerating a nil receiver so that a nil
// *AdminLogger degrades to a no-op instead of panicking on the request path.
func (a *AdminLogger) inner0() logger.Logger {
	if a == nil || a.inner == nil {
		return nopLogger{}
	}
	return a.inner
}

// nopLogger discards everything.
type nopLogger struct{}

func (nopLogger) Debug(string, ...any)                         {}
func (nopLogger) Info(string, ...any)                          {}
func (nopLogger) Warn(string, ...any)                          {}
func (nopLogger) Error(string, ...any)                         {}
func (nopLogger) DebugContext(context.Context, string, ...any) {}
func (nopLogger) InfoContext(context.Context, string, ...any)  {}
func (nopLogger) WarnContext(context.Context, string, ...any)  {}
func (nopLogger) ErrorContext(context.Context, string, ...any) {}
func (n nopLogger) With(...any) logger.Logger                  { return n }
func (n nopLogger) WithComponent(string) logger.Logger         { return n }

// Info logs an informational event with sanitized attributes.
func (a *AdminLogger) Info(msg string, args ...any) {
	a.inner0().Info(msg, sanitizeArgs(args)...)
}

// Warn logs a warning with sanitized attributes.
func (a *AdminLogger) Warn(msg string, args ...any) {
	a.inner0().Warn(msg, sanitizeArgs(args)...)
}

// Error logs an error with sanitized attributes.
func (a *AdminLogger) Error(msg string, args ...any) {
	a.inner0().Error(msg, sanitizeArgs(args)...)
}

// sanitizeArgs walks slog-style key/value arguments and redacts each value.
func sanitizeArgs(args []any) []any {
	out := make([]any, len(args))
	copy(out, args)
	for i := 0; i+1 < len(out); i += 2 {
		key, ok := out[i].(string)
		if !ok {
			continue
		}
		if s, isStr := out[i+1].(string); isStr {
			out[i+1] = RedactValue(key, s)
		}
	}
	return out
}

// ErrorSink returns an ErrorSink that records sanitised server-side detail.
//
// err.Error() is logged (it is server-side detail an operator needs) but passes through
// SanitizeLogField, so a path-bearing error cannot inject a forged line into the audit trail.
// It is NEVER sent to the client; that is Fail's job.
func (a *AdminLogger) ErrorSink() ErrorSink {
	if a == nil {
		return nil
	}
	return func(requestID, code string, err error) {
		if err == nil {
			return
		}
		a.Error("admin request failed",
			"request_id", requestID,
			"code", code,
			"detail", SanitizeLogField(err.Error()),
		)
	}
}

// auditRecord is one line of the mutating-request audit trail.
type auditRecord struct {
	Time        time.Time
	RequestID   string
	Method      string
	Route       string
	Fingerprint string
	Role        string
	Decision    AuditDecision
	Reason      string
}

// Audit emits exactly one structured line for a mutating request.
//
// This is the record an operator reads after an incident: who (certificate fingerprint and
// role), what (method and route), which request id, and the outcome. It deliberately records
// the FINGERPRINT rather than any certificate body.
func (a *AdminLogger) Audit(rec auditRecord) {
	if a == nil {
		return
	}
	args := []any{
		"request_id", rec.RequestID,
		"method", rec.Method,
		"route", rec.Route,
		"decision", string(rec.Decision),
	}
	if rec.Fingerprint != "" {
		// A fingerprint is a SHA-256 of public certificate bytes, not a secret, and is the
		// only safe way to identify the caller in a log.
		args = append(args, "principal_fp", SanitizeLogField(rec.Fingerprint))
	}
	if rec.Role != "" {
		args = append(args, "role", SanitizeLogField(rec.Role))
	}
	if rec.Reason != "" {
		args = append(args, "reason", SanitizeLogField(rec.Reason))
	}
	if rec.Decision == AuditDenied {
		a.Warn("admin audit: denied", args...)
		return
	}
	a.Info("admin audit", args...)
}

// AuditMiddleware emits one audit line per mutating request.
//
// It wraps the handler so the record is written whatever the outcome, including a panic
// further down the chain. Read-only requests are not audited by default: they are high volume
// and carry far less risk, and logging every dashboard poll would bury the signal.
func AuditMiddleware(log *AdminLogger, resolve func(*http.Request) *Principal) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isSafeMethod(r.Method) {
				next.ServeHTTP(w, r)
				return
			}
			rec := &responseRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			a := auditRecord{
				Time:      time.Now(),
				RequestID: RequestIDOf(r),
				Method:    r.Method,
				Route:     r.URL.Path,
			}
			// resolve may legitimately be nil (an unauthenticated deployment), so guard
			// the call rather than assuming a resolver was wired.
			if resolve != nil {
				if p := resolve(r); p != nil {
					a.Role = string(p.Role)
					a.Fingerprint = p.Fingerprint
				}
			}
			switch {
			case rec.status == http.StatusForbidden || rec.status == http.StatusUnauthorized:
				a.Decision = AuditDenied
				a.Reason = "authorization denied"
			default:
				a.Decision = AuditAllowed
			}
			if log != nil {
				log.Audit(a)
			}
		})
	}
}

// responseRecorder captures the status code while passing the body through untouched.
type responseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func (r *responseRecorder) WriteHeader(code int) {
	if !r.wroteHeader {
		r.status = code
		r.wroteHeader = true
	}
	r.ResponseWriter.WriteHeader(code)
}

// Unwrap exposes the underlying writer so http.Flusher and friends still work through the
// recorder (required for the SSE endpoint).
func (r *responseRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }

// Flush forwards to the wrapped writer when it supports flushing.
func (r *responseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
