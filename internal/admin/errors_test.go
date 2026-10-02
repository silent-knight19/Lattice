package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	latticeerrors "github.com/silent-knight19/lattice/internal/errors"
)

// TestCodeForError_Mapping verifies the internal-error to opaque-code mapping.
func TestCodeForError_Mapping(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want string
	}{
		{"nil", nil, ""},
		{"path", latticeerrors.ErrInvalidPath, CodeInvalidPath},
		{"path typed", &latticeerrors.InvalidPathError{Path: "/etc/passwd", Root: "/data", Reason: "outside root"}, CodeInvalidPath},
		{"key not found", latticeerrors.ErrKeyNotFound, CodeNotFound},
		{"key too large", latticeerrors.ErrKeyTooLarge, CodeTooLarge},
		{"value too large", latticeerrors.ErrValueTooLarge, CodeTooLarge},
		{"checksum", latticeerrors.ErrChecksumMismatch, CodeCorrupted},
		{"torn write", latticeerrors.ErrTornWrite, CodeCorrupted},
		{"raft corrupted", latticeerrors.ErrRaftCorruptedState, CodeCorrupted},
		{"raft poisoned", latticeerrors.ErrRaftStoragePoisoned, CodeCorrupted},
		{"write throttled", latticeerrors.ErrWriteThrottled, CodeBusy},
		{"queue full", latticeerrors.ErrQueueFull, CodeBusy},
		{"memory limit", latticeerrors.ErrMemoryLimitExceeded, CodeUnavailable},
		{"empty key", latticeerrors.ErrEmptyKey, CodeInvalidRequest},
		{"unknown", fmt.Errorf("something entirely unexpected"), CodeInternal},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := CodeForError(tc.err); got != tc.want {
				t.Errorf("CodeForError(%v) = %q, want %q", tc.err, got, tc.want)
			}
		})
	}
}

// TestCodeForError_Wrapped verifies errors.Is unwrapping, which is how the codebase wraps.
func TestCodeForError_Wrapped(t *testing.T) {
	wrapped := fmt.Errorf("reading sstable: %w",
		fmt.Errorf("footer: %w", latticeerrors.ErrChecksumMismatch))
	if got := CodeForError(wrapped); got != CodeCorrupted {
		t.Errorf("wrapped error mapped to %q, want %q", got, CodeCorrupted)
	}
}

// TestFail_NeverLeaksInternalDetail is the central SEC-9.3 assertion.
//
// A path-bearing internal error must produce a response containing none of: the path, the
// root, the reason, the raw Go error text, or any file path.
func TestFail_NeverLeaksInternalDetail(t *testing.T) {
	hostile := &latticeerrors.InvalidPathError{
		Path:   "/etc/shadow",
		Root:   "/var/lib/lattice/data",
		Reason: "path escapes root via symlink /tmp/evil -> /etc",
	}
	rec := httptest.NewRecorder()
	Fail(rec, hostile)

	body := rec.Body.String()
	for _, secret := range []string{
		"/etc/shadow", "/var/lib/lattice/data", "/tmp/evil",
		"symlink", "escapes root", "InvalidPathError", "lattice/internal/errors",
	} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q:\n%s", secret, body)
		}
	}
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
	// The code is present and useful.
	if !strings.Contains(body, `"code":"invalid_path"`) {
		t.Errorf("response lacks the opaque code:\n%s", body)
	}
}

// TestFail_UnknownErrorIsFullyOpaque verifies a completely unrecognized error leaks nothing
// at all, not even its own message.
func TestFail_UnknownErrorIsFullyOpaque(t *testing.T) {
	rec := httptest.NewRecorder()
	Fail(rec, fmt.Errorf("dial tcp 10.1.2.3:9099: connect: connection refused"))

	body := rec.Body.String()
	for _, secret := range []string{"10.1.2.3", "9099", "connection refused", "dial tcp"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q:\n%s", secret, body)
		}
	}
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
	if !strings.Contains(body, `"message":"internal error"`) {
		t.Errorf("expected the static internal message:\n%s", body)
	}
}

// TestFail_NeverLeaksKeyOrValueMaterial verifies user data cannot escape through an error.
func TestFail_NeverLeaksKeyOrValueMaterial(t *testing.T) {
	rec := httptest.NewRecorder()
	Fail(rec, fmt.Errorf("key %q with value %q rejected", "user:alice:secret", "hunter2"))

	body := rec.Body.String()
	for _, secret := range []string{"alice", "hunter2", "secret", "rejected"} {
		if strings.Contains(body, secret) {
			t.Errorf("response leaks %q:\n%s", secret, body)
		}
	}
}

// TestStaticMessages_NoSensitiveContent inspects the entire static message table.
func TestStaticMessages_NoSensitiveContent(t *testing.T) {
	for code, msg := range staticMessages {
		if strings.ContainsAny(msg, "/\\") {
			t.Errorf("message for %q contains a path separator: %q", code, msg)
		}
		if strings.Contains(msg, "{") || strings.Contains(msg, "%") {
			t.Errorf("message for %q looks like a template: %q", code, msg)
		}
	}
	// An unknown code must still resolve to safe static text, never empty.
	if got := messageFor("totally-unknown-code"); got != staticMessages[CodeInternal] {
		t.Errorf("messageFor(unknown) = %q, want the internal fallback", got)
	}
}

// TestWriteCoded_EnvelopeShape pins the wire format.
func TestWriteCoded_EnvelopeShape(t *testing.T) {
	rec := httptest.NewRecorder()
	WriteCoded(rec, 0, CodeNotFound)

	body := strings.TrimSpace(rec.Body.String())
	want := `{"error":{"code":"not_found","message":"resource not found"}}`
	if body != want {
		t.Errorf("body  = %s\nwant = %s", body, want)
	}
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
	if rec.Header().Get("Content-Type") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing security headers on the error envelope")
	}
}

// TestStatusForCode covers the code-to-status pairing.
func TestStatusForCode(t *testing.T) {
	cases := map[string]int{
		CodeInvalidRequest: http.StatusBadRequest,
		CodeInvalidPath:    http.StatusBadRequest,
		CodeNotFound:       http.StatusNotFound,
		CodeForbidden:      http.StatusForbidden,
		CodeConflict:       http.StatusConflict,
		CodeTooLarge:       http.StatusRequestEntityTooLarge,
		CodeCorrupted:      http.StatusUnprocessableEntity,
		CodeBusy:           http.StatusServiceUnavailable,
		CodeNotSupported:   http.StatusNotImplemented,
		CodeTimeout:        http.StatusGatewayTimeout,
		CodeInternal:       http.StatusInternalServerError,
		"unmapped":         http.StatusInternalServerError,
	}
	for code, want := range cases {
		if got := statusForCode(code); got != want {
			t.Errorf("statusForCode(%q) = %d, want %d", code, got, want)
		}
	}
}

// TestRequestID_AlwaysPresentAndUnique verifies SEC-9.4.
func TestRequestID_AlwaysPresentAndUnique(t *testing.T) {
	seen := make(map[string]bool)
	for i := 0; i < 256; i++ {
		rec := httptest.NewRecorder()
		var captured string
		h := RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			captured = RequestIDOf(r)
			w.WriteHeader(http.StatusOK)
		}))
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

		id := rec.Header().Get(RequestIDHeader)
		if id == "" {
			t.Fatal("X-Request-Id header missing")
		}
		if len(id) != 32 {
			t.Errorf("request id %q is not 32 hex chars", id)
		}
		if captured != id {
			t.Errorf("context id %q != header id %q", captured, id)
		}
		if seen[id] {
			t.Fatalf("duplicate request id %q", id)
		}
		seen[id] = true
	}
}

// TestRequestID_IgnoresInboundHeader pins the anti-spoofing decision.
func TestRequestID_IgnoresInboundHeader(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	req.Header.Set(RequestIDHeader, "attacker-supplied-id")
	rec := httptest.NewRecorder()

	var got string
	RequestID(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = RequestIDOf(r)
	})).ServeHTTP(rec, req)

	if got == "attacker-supplied-id" {
		t.Error("inbound X-Request-Id was trusted; log injection is possible")
	}
	if rec.Header().Get(RequestIDHeader) == "attacker-supplied-id" {
		t.Error("inbound X-Request-Id was echoed back")
	}
}

// TestFailRequest_LogsDetailWithID verifies SEC-9.3's "log server-side with the request id"
// half: the detail must reach the sink, keyed by id, while the response stays opaque.
func TestFailRequest_LogsDetailWithID(t *testing.T) {
	var gotID, gotCode string
	var gotErr error
	sink := func(id, code string, err error) {
		gotID, gotCode, gotErr = id, code, err
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/sstables/inspect?file=x", nil)
	req = req.WithContext(WithRequestID(req.Context(), "deadbeefdeadbeefdeadbeefdeadbeef"))

	rec := httptest.NewRecorder()
	internal := &latticeerrors.InvalidPathError{Path: "/etc/passwd", Root: "/data", Reason: "escape"}
	FailRequest(rec, req, internal, sink)

	// Server side sees the real detail.
	if gotErr == nil {
		t.Fatal("sink received no error detail")
	}
	if !strings.Contains(gotErr.Error(), "/etc/passwd") {
		t.Errorf("sink detail lost the original error: %v", gotErr)
	}
	if gotID != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("sink id = %q, want the request id", gotID)
	}
	if gotCode != CodeInvalidPath {
		t.Errorf("sink code = %q, want %q", gotCode, CodeInvalidPath)
	}

	// Client side sees none of it.
	if strings.Contains(rec.Body.String(), "/etc/passwd") {
		t.Errorf("client response leaked the path:\n%s", rec.Body.String())
	}
	if rec.Header().Get(RequestIDHeader) == "" {
		t.Log("note: FailRequest does not itself set the header; RequestID middleware does")
	}
}

// TestFailRequest_NilSinkIsSafe ensures a nil sink cannot panic.
func TestFailRequest_NilSinkIsSafe(t *testing.T) {
	rec := httptest.NewRecorder()
	FailRequest(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil),
		latticeerrors.ErrKeyNotFound, nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestFail_NilErrorIsSafe ensures Fail(nil) does not panic.
func TestFail_NilErrorIsSafe(t *testing.T) {
	rec := httptest.NewRecorder()
	Fail(rec, nil)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", rec.Code)
	}
}

// TestRequestIDFrom_NilSafe guards shutdown paths.
func TestRequestIDFrom_NilSafe(t *testing.T) {
	if RequestIDFrom(nil) != "" {
		t.Error("RequestIDFrom(nil) should be empty")
	}
	if RequestIDOf(nil) != "" {
		t.Error("RequestIDOf(nil) should be empty")
	}
}
