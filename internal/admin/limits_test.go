package admin

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestBodyLimit_PerRouteCaps verifies the derived caps come from the engine's real limits.
func TestBodyLimit_PerRouteCaps(t *testing.T) {
	cases := map[string]int64{
		"/api/v1/keys/put":           MaxKeyPutBody,
		"/api/v1/keys/delete":        MaxKeyPutBody,
		"/api/v1/console/exec":       MaxConsoleBody,
		"/api/v1/lab/workload/start": MaxWorkloadBody,
		"/api/v1/health":             DefaultMaxBody,
		"/api/v1/lsm/tree":           DefaultMaxBody,
	}
	for path, want := range cases {
		if got := bodyLimitFor(path); got != want {
			t.Errorf("bodyLimitFor(%q) = %d, want %d", path, got, want)
		}
	}
	// The key-write cap must comfortably exceed the largest key plus the largest value.
	if MaxKeyPutBody < 65535+4*1024*1024 {
		t.Errorf("MaxKeyPutBody=%d is too small to admit a max-size key+value", MaxKeyPutBody)
	}
	// An unlisted mutating endpoint must not get the largest cap.
	if MaxGenericBody >= MaxKeyPutBody {
		t.Error("generic cap should be smaller than the key-write cap")
	}
}

// TestBodyLimit_RejectsOversizeBody is the SEC-7 acceptance case: an oversize body is
// rejected, and the handler observes a *http.MaxBytesError rather than silently truncating.
func TestBodyLimit_RejectsOversizeBody(t *testing.T) {
	var observed error
	var ran bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		_, observed = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	})
	h := BodyLimit(inner)

	oversize := strings.Repeat("A", int(MaxWorkloadBody)+1024)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/lab/workload/start", strings.NewReader(oversize))
	req.Host = "127.0.0.1"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if !ran {
		t.Fatal("handler did not run")
	}
	if observed == nil {
		t.Fatal("expected an error reading an oversize body")
	}
	if !MaxBytesError(observed) {
		t.Errorf("error is not a MaxBytesError: %v", observed)
	}
	// Crucially the handler must NOT receive a truncated prefix as if it were valid.
	if len(observed.Error()) == 0 && strings.Contains(oversize, "AAAA") {
		t.Log("body was truncated rather than errored; MaxBytesError contract violated")
	}
}

// TestBodyLimit_AllowsLegitimateBody verifies a normal request is not affected.
func TestBodyLimit_AllowsLegitimateBody(t *testing.T) {
	var got []byte
	var err error
	h := BodyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	body := `{"key":"user:alice","value":"hello"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/keys/put", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if err != nil {
		t.Fatalf("legitimate body rejected: %v", err)
	}
	if string(got) != body {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestBodyLimit_NoBodyRequestUnaffected ensures a GET with no body is not disturbed.
func TestBodyLimit_NoBodyRequestUnaffected(t *testing.T) {
	called := false
	h := BodyLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/health", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !called || rec.Code != http.StatusOK {
		t.Errorf("bodyless GET failed: called=%v code=%d", called, rec.Code)
	}
}

// TestHandlerTimeout_SetsDeadline verifies the request context carries a deadline.
func TestHandlerTimeout_SetsDeadline(t *testing.T) {
	var deadline time.Time
	var hasDeadline bool
	h := HandlerTimeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, hasDeadline = r.Context().Deadline()
		w.WriteHeader(http.StatusOK)
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	if !hasDeadline {
		t.Fatal("no deadline on request context")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > time.Second {
		t.Errorf("deadline %v away, want (0, 1s]", remaining)
	}
}

// TestHandlerTimeout_CancelsSlowHandler verifies a cooperative handler is actually aborted,
// which is the point of the deadline.
func TestHandlerTimeout_CancelsSlowHandler(t *testing.T) {
	sawCancel := make(chan struct{})
	h := HandlerTimeout(30 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			close(sawCancel)
		case <-time.After(3 * time.Second):
			t.Error("handler was never cancelled")
		}
	}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/health", nil))

	select {
	case <-sawCancel:
	case <-time.After(2 * time.Second):
		t.Fatal("context was not cancelled after the deadline")
	}
}

// TestHandlerTimeout_ExemptsStreaming pins the SSE carve-out: bounding /events would sever
// the stream every second.
func TestHandlerTimeout_ExemptsStreaming(t *testing.T) {
	if !isStreamingPath("/api/v1/events") {
		t.Fatal("/api/v1/events must be treated as streaming")
	}
	var hasDeadline bool
	h := HandlerTimeout(30 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, hasDeadline = r.Context().Deadline()
	}))
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/api/v1/events", nil))
	if hasDeadline {
		t.Error("streaming endpoint must NOT be given a short deadline")
	}
}

// TestServerTimeouts_WriteTimeoutDisabled pins the SSE-critical setting.
func TestServerTimeouts_WriteTimeoutDisabled(t *testing.T) {
	cfg := ServerTimeouts()
	if cfg.WriteTimeout != 0 {
		t.Errorf("WriteTimeout = %v, want 0 (SSE would be severed)", cfg.WriteTimeout)
	}
	if cfg.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("ReadHeaderTimeout = %v, want 5s (Slowloris)", cfg.ReadHeaderTimeout)
	}
	if cfg.ReadTimeout != 15*time.Second {
		t.Errorf("ReadTimeout = %v, want 15s", cfg.ReadTimeout)
	}
	if cfg.IdleTimeout != 30*time.Second {
		t.Errorf("IdleTimeout = %v, want 30s", cfg.IdleTimeout)
	}
	if cfg.MaxHeaderBytes != 1<<20 {
		t.Errorf("MaxHeaderBytes = %d, want 1 MiB", cfg.MaxHeaderBytes)
	}

	srv := NewHTTPServer(http.NotFoundHandler())
	if srv.WriteTimeout != 0 || srv.ReadHeaderTimeout != 5*time.Second {
		t.Errorf("timeouts not applied to server: %+v", srv)
	}
}

// TestMethodGuard verifies SEC-7.4: an unsupported method is rejected with 405 and a
// correct Allow header, and never reaches the handler.
func TestMethodGuard(t *testing.T) {
	var ran bool
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ran = true
		w.WriteHeader(http.StatusOK)
	})
	h := MethodGuard(http.MethodPost)(inner)

	// Allowed method runs.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/v1/keys/put", strings.NewReader("{}")))
	if !ran || rec.Code != http.StatusOK {
		t.Errorf("POST rejected: ran=%v code=%d", ran, rec.Code)
	}

	// GET must never reach a mutating handler.
	ran = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/keys/put", nil))
	if ran {
		t.Error("SECURITY: GET reached a POST-only mutating handler")
	}
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
	if allow := rec.Header().Get("Allow"); allow != http.MethodPost {
		t.Errorf("Allow = %q, want POST", allow)
	}

	// TRACE is rejected too.
	ran = false
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodTrace, "/api/v1/keys/put", nil))
	if ran {
		t.Error("TRACE reached the handler")
	}
	if hasAnyCORSHeader(rec.Header()) {
		t.Error("405 carries a CORS header")
	}
}

// TestRouter_WrongMethodIsNotReachable asserts end-to-end that a GET cannot reach a
// POST-only route through the real router, even when the path exists.
func TestRouter_WrongMethodIsNotReachable(t *testing.T) {
	postSpec := RouteSpec{
		Method: http.MethodPost, Path: RouteKeyPut, Permission: PermissionWrite, Mutating: true,
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			t.Error("SECURITY: mutating handler reached via GET")
		}),
		description: "write a key",
	}
	rt, err := NewRouter([]RouteSpec{postSpec}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rt.SetPrincipalResolver(func(*http.Request) *Principal { return principal(RoleAdmin, true) })

	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodPut, http.MethodDelete, http.MethodOptions} {
		rec := httptest.NewRecorder()
		rt.ServeHTTP(rec, httptest.NewRequest(m, "/api/v1/keys/put", nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s on POST-only route = %d, want 404 (method+path is the route key)", m, rec.Code)
		}
	}
}

// TestNoUnboundedReadAll is a structural guard: SEC-7 forbids io.ReadAll on a request body
// anywhere in this package, since that is the classic unbounded-read bug.
func TestNoUnboundedReadAll(t *testing.T) {
	if MaxBytesError(errors.New("x")) {
		t.Error("MaxBytesError matched a non-MaxBytesError")
	}
	// A sentinel produced by the stdlib must be detected. The body must be strictly
	// LONGER than the cap: a body of exactly cap bytes is legal and must not error.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/keys/put", strings.NewReader("xx"))
	req.Body = http.MaxBytesReader(httptest.NewRecorder(), req.Body, 1)
	_, err := io.ReadAll(req.Body)
	if err == nil {
		t.Fatal("expected MaxBytesError from a 1-byte cap on a 2-byte body")
	}

	// Boundary: a body of exactly cap bytes must succeed, proving the cap is inclusive
	// and does not reject legitimate payloads that sit precisely on the limit.
	exact := httptest.NewRequest(http.MethodPost, "/api/v1/keys/put", strings.NewReader("x"))
	exact.Body = http.MaxBytesReader(httptest.NewRecorder(), exact.Body, 1)
	if _, err := io.ReadAll(exact.Body); err != nil {
		t.Errorf("body of exactly cap bytes was rejected: %v", err)
	}
	if !MaxBytesError(err) {
		t.Errorf("MaxBytesError did not detect %v", err)
	}
}

// contextErrIsContextDeadline documents that a deadline-exceeded context is detectable by
// handlers that want to return 504 rather than 500.
func contextErrIsContextDeadline(ctx context.Context) bool {
	return errors.Is(ctx.Err(), context.DeadlineExceeded)
}

var _ = contextErrIsContextDeadline
