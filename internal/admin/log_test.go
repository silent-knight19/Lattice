package admin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/silent-knight19/lattice/internal/logger"
)

// syncBuffer is a concurrency-safe sink for capturing log output.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func (s *syncBuffer) Lines() []string {
	out := strings.Split(strings.TrimSpace(s.String()), "\n")
	if len(out) == 1 && out[0] == "" {
		return nil
	}
	return out
}

// newCapturingLogger builds an AdminLogger writing JSON into a capture buffer.
func newCapturingLogger() (*AdminLogger, *syncBuffer) {
	buf := &syncBuffer{}
	return NewAdminLogger(logger.NewJSON(buf, logger.LevelDebug)), buf
}

// TestSanitizeLogField covers control stripping, ANSI removal and truncation.
func TestSanitizeLogField(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "hello", "hello"},
		{"empty", "", ""},
		{"newline stripped", "a\nFAKE", "aFAKE"},
		{"carriage return stripped", "a\r\nb", "ab"},
		{"ansi stripped", "a\x1b[31mRED\x1b[0m", "a[31mRED[0m"},
		{"null stripped", "a\x00b", "ab"},
		{"unicode preserved", "héllo→世界", "héllo→世界"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := SanitizeLogField(tc.in); got != tc.want {
				t.Errorf("SanitizeLogField(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestSanitizeLogField_Truncates verifies the log-flooding mitigation.
func TestSanitizeLogField_Truncates(t *testing.T) {
	long := strings.Repeat("A", 100000)
	got := SanitizeLogField(long)
	if len(got) != MaxLogFieldLen {
		t.Errorf("truncated length = %d, want %d", len(got), MaxLogFieldLen)
	}
}

// TestSanitizeLogField_TruncationKeepsValidUTF8 verifies no rune is split in half.
func TestSanitizeLogField_TruncationKeepsValidUTF8(t *testing.T) {
	// Multi-byte runes right at the boundary.
	got := SanitizeLogField(strings.Repeat("世", 200))
	if len(got) > MaxLogFieldLen+4 {
		t.Errorf("length %d exceeds cap", len(got))
	}
	if !isValidUTF8(got) {
		t.Errorf("truncation produced invalid UTF-8: %q", got)
	}
}

func isValidUTF8(s string) bool {
	for _, r := range s {
		if r == '\uFFFD' && !strings.Contains(s, "�") {
			return false
		}
	}
	return true
}

// TestRedactValue_SensitiveKeys covers SEC-10.3 by field name.
func TestRedactValue_SensitiveKeys(t *testing.T) {
	for _, key := range []string{
		"csrf_token", "CSRF_TOKEN", " csrf ", "tls_key", "private_key",
		"cert", "cert_pem", "certificate", "authorization", "cookie",
	} {
		if got := RedactValue(key, "SUPERSECRET"); got != RedactedValue {
			t.Errorf("RedactValue(%q) = %q, want REDACTED", key, got)
		}
	}
	// Ordinary fields must survive.
	if got := RedactValue("key", "user:alice"); got != "user:alice" {
		t.Errorf("ordinary value was redacted: %q", got)
	}
}

// TestRedactValue_PEMByShape covers the value-semantic redaction the base logger lacks.
func TestRedactValue_PEMByShape(t *testing.T) {
	pem := "-----BEGIN CERTIFICATE-----\nMIIBkTCB+wIJAKZ\n-----END CERTIFICATE-----"
	if got := RedactValue("innocent_field", pem); got != RedactedValue {
		t.Errorf("PEM under a neutral field name was not redacted: %q", got)
	}
	key := "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADAN\n-----END PRIVATE KEY-----"
	if got := RedactValue("note", key); got != RedactedValue {
		t.Errorf("private key PEM was not redacted: %q", got)
	}
}

// TestRedactValue_HighEntropyToken closes the measured gap where the same secret leaked
// under a neutral field name.
func TestRedactValue_HighEntropyToken(t *testing.T) {
	// Shape of the real CSRF token: 43 chars base64url.
	token := "g697AKBRy5rvYSeKiTd1C37UbqedFyo4viKopUQ_ZIA"
	if got := RedactValue("request_id", token); got != RedactedValue {
		t.Errorf("high-entropy token under a neutral key was NOT redacted: %q", got)
	}
	if got := RedactValue("csrf_token", token); got != RedactedValue {
		t.Errorf("token under a sensitive key was not redacted: %q", got)
	}
	// Hex identifiers are NOT secrets: redacting them would break the request-id
	// correlation the audit trail depends on. Regression test for a real bug where the
	// 32-char request id was classified as a token and the audit line lost it.
	for _, id := range []string{
		"3159e0e1d5c82f39a3d31a6fcc6ae18a",
		"DEADBEEFCAFEBABE0123456789ABCDEF0123",
		"sha256:abcdef0123456789abcdef0123456789abcdef0123456789abcdef01234567",
	} {
		if got := RedactValue("request_id", id); got == RedactedValue {
			t.Errorf("hex identifier was wrongly redacted: %q", id)
		}
	}
	// But a base64url token with the same length is still redacted.
	if got := RedactValue("request_id", "I96-bnhyYO1234567890abcdefgh-_1234567890"); got != RedactedValue {
		t.Errorf("base64url token was not redacted: %q", got)
	}

	// Ordinary long strings must NOT be redacted, or logs become useless.
	for _, benign := range []string{
		strings.Repeat("a", 100),
		"this is a perfectly ordinary long log message about a compaction that ran for a while",
		"GET /api/v1/lsm/tree returned 36 files across 7 levels in 12 milliseconds",
		"connection to 10.0.0.5:9099 refused after 3 attempts, retrying with backoff",
	} {
		if got := RedactValue("detail", benign); got == RedactedValue {
			t.Errorf("benign value was wrongly redacted: %q", benign)
		}
	}
}

// TestAdminLogger_NeverLogsForgedLine is the SEC-10 acceptance case: a key containing a
// newline plus a fake log line must produce exactly ONE output line.
func TestAdminLogger_NeverLogsForgedLine(t *testing.T) {
	for _, format := range []string{"json", "text"} {
		t.Run(format, func(t *testing.T) {
			buf := &syncBuffer{}
			var l logger.Logger
			if format == "json" {
				l = logger.NewJSON(buf, logger.LevelDebug)
			} else {
				l = logger.NewText(buf, logger.LevelDebug)
			}
			al := NewAdminLogger(l)
			al.Info("admin audit", "key", "alice\nINFO forged line\nsecond forged")

			lines := buf.Lines()
			if len(lines) != 1 {
				t.Fatalf("got %d log lines, want exactly 1:\n%s", len(lines), buf.String())
			}
			if strings.Contains(buf.String(), "forged line\nsecond") {
				t.Errorf("a forged multi-line record survived:\n%s", buf.String())
			}
		})
	}
}

// TestAdminLogger_NeverLogsSecrets is the other SEC-10 acceptance case: token values must
// never appear in captured output.
func TestAdminLogger_NeverLogsSecrets(t *testing.T) {
	const secret = "g697AKBRy5rvYSeKiTd1C37UbqedFyo4viKopUQ_ZIA"
	al, buf := newCapturingLogger()

	al.Info("with sensitive key", "csrf_token", secret)
	al.Info("with neutral key", "request_id", secret)
	al.Info("with tls key name", "tls_key", secret)
	al.Info("with cert", "cert_pem", "-----BEGIN CERTIFICATE-----"+secret)
	al.Warn("warned", "csrf_token", secret)
	al.Error("errored", "csrf_token", secret)

	out := buf.String()
	if strings.Contains(out, secret) {
		t.Errorf("SECURITY: token leaked into log output:\n%s", out)
	}
	if !strings.Contains(out, RedactedValue) {
		t.Errorf("expected redaction placeholders:\n%s", out)
	}
}

// TestAdminLogger_TruncatesHugeValues proves the flooding mitigation end to end.
func TestAdminLogger_TruncatesHugeValues(t *testing.T) {
	al, buf := newCapturingLogger()
	al.Info("huge", "detail", strings.Repeat("Q", 2*1024*1024))

	out := buf.String()
	if len(out) > 2000 {
		t.Errorf("log line is %d bytes; a 2 MiB value was not truncated", len(out))
	}
	if !strings.Contains(out, strings.Repeat("Q", 64)) {
		t.Error("expected the truncated prefix to be present")
	}
}

// TestAdminLogger_ErrorSink verifies SEC-9's server-side detail path is now supplied.
func TestAdminLogger_ErrorSink(t *testing.T) {
	al, buf := newCapturingLogger()
	sink := al.ErrorSink()
	if sink == nil {
		t.Fatal("ErrorSink returned nil")
	}
	sink("req-123", CodeInvalidPath, fmt.Errorf("invalid path %q outside root %q", "/etc/shadow", "/data"))

	out := buf.String()
	if !strings.Contains(out, "req-123") {
		t.Errorf("request id missing from log:\n%s", out)
	}
	if !strings.Contains(out, "invalid_path") {
		t.Errorf("code missing from log:\n%s", out)
	}
	if !strings.Contains(out, "/etc/shadow") {
		t.Errorf("server-side detail should be present in the log:\n%s", out)
	}
	// A nil error must not panic or log.
	sink("req-124", CodeInternal, nil)
}

// TestErrorSink_ForgedLineInDetail verifies error text cannot forge an audit line.
func TestErrorSink_ForgedLineInDetail(t *testing.T) {
	al, buf := newCapturingLogger()
	al.ErrorSink()("id", "code", fmt.Errorf("bad thing\n{\"level\":\"INFO\",\"msg\":\"forged\"}"))

	lines := buf.Lines()
	if len(lines) != 1 {
		t.Fatalf("error detail forged %d lines, want 1:\n%s", len(lines), buf.String())
	}
	for _, line := range lines {
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Errorf("line is not valid JSON after injection attempt: %s", line)
		}
	}
}

// TestAuditMiddleware_EmitsOneLinePerMutation is the SEC-10.4 acceptance case.
func TestAuditMiddleware_EmitsOneLinePerMutation(t *testing.T) {
	al, buf := newCapturingLogger()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

	h := AuditMiddleware(al, func(*http.Request) *Principal {
		return &Principal{Role: RoleAdmin, Fingerprint: "abc123", Authenticated: true}
	})(ok)

	// Mutating request.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/keys/put", nil)
	req = req.WithContext(WithRequestID(req.Context(), "rid-1"))
	h.ServeHTTP(rec, req)

	if len(buf.Lines()) != 1 {
		t.Fatalf("want exactly 1 audit line, got %d:\n%s", len(buf.Lines()), buf.String())
	}
	line := buf.String()
	for _, want := range []string{"rid-1", "POST", "/api/v1/keys/put", "allowed", "abc123", "admin"} {
		if !strings.Contains(line, want) {
			t.Errorf("audit line missing %q:\n%s", want, line)
		}
	}

	// Read-only request must not be audited by default.
	buf2 := &syncBuffer{}
	al2 := NewAdminLogger(logger.NewJSON(buf2, logger.LevelDebug))
	h2 := AuditMiddleware(al2, func(*http.Request) *Principal { return nil })(ok)
	h2.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))
	if buf2.String() != "" {
		t.Errorf("read-only request produced an audit line:\n%s", buf2.String())
	}
}

// TestAuditMiddleware_RecordsDenial verifies a refused mutation is audited as denied.
func TestAuditMiddleware_RecordsDenial(t *testing.T) {
	al, buf := newCapturingLogger()
	forbidden := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeForbiddenAuthz(w)
	})
	h := AuditMiddleware(al, func(*http.Request) *Principal {
		return &Principal{Role: RoleReader, Authenticated: true}
	})(forbidden)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/lab/crash", nil))

	line := buf.String()
	if !strings.Contains(line, "denied") {
		t.Errorf("denial not recorded:\n%s", line)
	}
	if !strings.Contains(line, "/api/v1/lab/crash") {
		t.Errorf("route not recorded:\n%s", line)
	}
}

// TestAuditMiddleware_NilLoggerIsSafe ensures audit logging cannot break the request path.
func TestAuditMiddleware_NilLoggerIsSafe(t *testing.T) {
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := AuditMiddleware(nil, nil)(ok)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/keys/put", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
}

// TestResponseRecorder_PreservesFlushing keeps SSE working through the audit wrapper.
func TestResponseRecorder_PreservesFlushing(t *testing.T) {
	al := NewAdminLogger(nil)
	flushed := false
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f, ok := w.(http.Flusher)
		if !ok {
			t.Error("ResponseWriter does not implement http.Flusher")
			return
		}
		f.Flush()
		flushed = true
	})
	AuditMiddleware(al, nil)(inner).ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPost, "/api/v1/x", nil))
	if !flushed {
		t.Error("flush did not reach the wrapped writer")
	}
}

// TestNewAdminLogger_NilSafe verifies a nil inner logger is replaced, not dereferenced.
func TestNewAdminLogger_NilSafe(t *testing.T) {
	al := NewAdminLogger(nil)
	al.Info("ok", "k", "v")
	al.Warn("ok")
	al.Error("ok")
	if NewAdminLogger(nil).ErrorSink() == nil {
		t.Error("ErrorSink should be non-nil even with a nil inner logger")
	}
	var nilLogger *AdminLogger
	nilLogger.Info("must not panic")
	nilLogger.Audit(auditRecord{})
	if nilLogger.ErrorSink() != nil {
		t.Error("nil AdminLogger ErrorSink should return nil")
	}
}
