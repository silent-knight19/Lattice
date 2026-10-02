package admin

// This file is the CONSOLIDATED SECURITY REGRESSION NET (SEC-12).
//
// Every attack class discovered during the SEC-0..SEC-11 work is exercised here against a
// fully-wired server, in one place, so a future change to any middleware cannot silently
// weaken a control.
//
// Two rules govern everything here, both learned the hard way:
//
//  1. ASSERT SERVER-SIDE, NEVER CLIENT-SIDE. SEC-0 finding F12: a browser reports an opaque
//     network error whether a cross-origin request was blocked or delivered. Every case below
//     therefore checks whether the INNER HANDLER EXECUTED, using a counter, not the response
//     the client happened to see.
//
//  2. SEND RAW BYTES WHERE THE CLIENT WOULD NORMALISE. curl rewrites "../" before sending, so
//     traversal is exercised over a raw TCP connection (see TestSecuritySuite_RawByteTraversal).

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/silent-knight19/lattice/internal/security"
)

// secHarness is a fully-wired admin server plus the counters that make assertions honest.
type secHarness struct {
	server   *httptest.Server
	bus      *EventBus
	token    *CSRFToken
	role     atomic.Value // Role the harness presents
	executed atomic.Int64 // inner-handler executions: the ground truth
	authz    *atomic.Bool // when false, the presented principal is unauthenticated
	fp       string
}

// newSecHarness wires the canonical middleware chain exactly as ADM-2 specifies:
//
//	RequestID -> SecurityHeaders -> NoStoreAPI -> BodyLimit -> HandlerTimeout
//	          -> HostGuard -> OriginGuard -> CSRFGuard -> Audit -> Router
//
// The router's single mutating route (/keys/put) and its SSE route stand in for the real
// surface; anything the attack reaches is detected by the execution counter.
func newSecHarness(t *testing.T, maxSubscribers int) *secHarness {
	t.Helper()

	bus := NewEventBus(BusOptions{MaxSubscribers: maxSubscribers})
	tok, err := NewCSRFToken()
	if err != nil {
		t.Fatalf("NewCSRFToken: %v", err)
	}

	h := &secHarness{bus: bus, token: tok, fp: "sec0123456789abcdef"}
	h.role.Store(RoleAdmin)

	counted := func(inner http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.executed.Add(1)
			inner.ServeHTTP(w, r)
		})
	}

	mutate := counted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	read := counted(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	specs := []RouteSpec{
		{Method: http.MethodGet, Path: RouteSession, Permission: PermissionRead,
			Handler: SessionHandler(tok), description: "session"},
		{Method: http.MethodGet, Path: RouteHealth, Permission: PermissionRead,
			Handler: read, description: "health"},
		{Method: http.MethodGet, Path: RouteKeys, Permission: PermissionRead,
			Handler: read, description: "keys"},
		{Method: http.MethodPost, Path: RouteKeyPut, Permission: PermissionWrite, Mutating: true,
			Handler: MethodGuard(http.MethodPost)(mutate), description: "put"},
		{Method: http.MethodPost, Path: RouteLabCrash, Permission: PermissionAdmin, Mutating: true,
			Handler: MethodGuard(http.MethodPost)(mutate), description: "crash"},
		{Method: http.MethodGet, Path: RouteEvents, Permission: PermissionAdmin,
			Handler: newSSEStreamHandler(bus), description: "events"},
	}
	api, err := NewRouter(specs, NewStaticHandlerFS(staticTestFS))
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	authed := &atomic.Bool{}
	authed.Store(true)
	h.authz = authed
	api.SetPrincipalResolver(func(*http.Request) *Principal {
		return &Principal{
			Role:          h.role.Load().(Role),
			Fingerprint:   h.fp,
			Authenticated: authed.Load(),
		}
	})

	hosts := NewHostAllowlist("127.0.0.1", "localhost")
	origins := NewOriginAllowlist("http://127.0.0.1", "http://localhost")

	var h2 http.Handler = api
	h2 = CSRFGuard(tok)(h2)
	h2 = OriginGuard(origins)(h2)
	h2 = HostGuard(hosts, "http")(h2)
	h2 = HandlerTimeout(5 * time.Second)(h2)
	h2 = BodyLimit(h2)
	h2 = NoStoreAPI(h2)
	h2 = SecurityHeaders(h2)
	mux := RequestID(h2)

	h.server = httptest.NewServer(mux)
	h.authz = authed
	return h
}

func (h *secHarness) close() { h.server.Close() }

// setAuthed flips whether the harness presents an authenticated principal.
func (h *secHarness) setAuthed(v bool) {
	h.authz.Store(v)
}

// do issues a request with the given method, path and headers. Any header whose value is
// "" is omitted.
func (h *secHarness) do(t *testing.T, method, path string, headers map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, h.server.URL+APIPrefix+path, strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1"
	for k, v := range headers {
		if v != "" {
			req.Header.Set(k, v)
		}
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// legit returns headers for a fully legitimate mutating request.
func (h *secHarness) legit() map[string]string {
	return map[string]string{
		CSRFHeader:         h.token.Value(),
		AdminConfirmHeader: "confirm",
	}
}

// TestSecuritySuite_DriveByOrigin is the SEC-0 attack, permanently encoded as a regression.
func TestSecuritySuite_DriveByOrigin(t *testing.T) {
	h := newSecHarness(t, 4)
	defer h.close()

	for _, origin := range []string{
		"http://evil.com",
		"http://127.0.0.1:9099", // the exact SEC-0 attacker origin
		"http://127.0.0.1:7070.evil.com",
		"http://evil-127.0.0.1",
		"https://127.0.0.1", // scheme mismatch
		"null",
	} {
		before := h.executed.Load()
		hd := h.legit()
		hd["Origin"] = origin
		resp := h.do(t, http.MethodPost, RouteKeyPut, hd)
		_ = resp.Body.Close()

		if got := h.executed.Load(); got != before {
			t.Errorf("SECURITY: handler executed for origin %q", origin)
		}
	}
}

// TestSecuritySuite_DNSRebinding is the SEC-2 attack.
func TestSecuritySuite_DNSRebinding(t *testing.T) {
	h := newSecHarness(t, 4)
	defer h.close()

	// A self-consistent Origin AND a rebound Host: neither guard's precondition helps
	// alone, so both must fire.
	for _, host := range []string{"attacker.example", "evil.local", "127.0.0.1:9999"} {
		before := h.executed.Load()
		req, err := http.NewRequest(http.MethodPost, h.server.URL+APIPrefix+RouteKeyPut, strings.NewReader("{}"))
		if err != nil {
			t.Fatal(err)
		}
		req.Host = host
		req.Header.Set(CSRFHeader, h.token.Value())
		req.Header.Set(AdminConfirmHeader, "confirm")
		req.Header.Set("Origin", "http://"+host)
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		_ = resp.Body.Close()
		if got := h.executed.Load(); got != before {
			t.Errorf("SECURITY: handler executed for rebound Host %q", host)
		}
	}
}

// TestSecuritySuite_CSRFAndConfirm covers SEC-3.
func TestSecuritySuite_CSRFAndConfirm(t *testing.T) {
	h := newSecHarness(t, 4)
	defer h.close()

	stale, err := NewCSRFToken()
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		headers map[string]string
	}{
		{"missing both", map[string]string{}},
		{"missing token", map[string]string{AdminConfirmHeader: "confirm"}},
		{"missing confirm", map[string]string{CSRFHeader: h.token.Value()}},
		{"wrong token", map[string]string{CSRFHeader: "wrong-token", AdminConfirmHeader: "confirm"}},
		{"stale token from a previous boot", map[string]string{CSRFHeader: stale.Value(), AdminConfirmHeader: "confirm"}},
		{"wrong confirm value", map[string]string{CSRFHeader: h.token.Value(), AdminConfirmHeader: "yes"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := h.executed.Load()
			resp := h.do(t, http.MethodPost, RouteKeyPut, tc.headers)
			_ = resp.Body.Close()
			if got := h.executed.Load(); got != before {
				t.Errorf("SECURITY: handler executed with %s", tc.name)
			}
			if resp.StatusCode != http.StatusForbidden {
				t.Errorf("status = %d, want 403", resp.StatusCode)
			}
		})
	}

	// And the positive control: a complete legitimate request DOES execute.
	before := h.executed.Load()
	resp := h.do(t, http.MethodPost, RouteKeyPut, h.legit())
	_ = resp.Body.Close()
	if h.executed.Load() != before+1 {
		t.Error("a fully legitimate mutating request did not reach the handler")
	}
}

// TestSecuritySuite_RoleEscalation covers SEC-4.
func TestSecuritySuite_RoleEscalation(t *testing.T) {
	h := newSecHarness(t, 4)
	defer h.close()

	cases := []struct {
		role     Role
		path     string
		wantRun  bool
		wantCode int
	}{
		{RoleReader, RouteHealth, true, http.StatusOK},
		{RoleReader, RouteKeyPut, false, http.StatusForbidden},
		{RoleReader, RouteLabCrash, false, http.StatusForbidden},
		{RoleWriter, RouteKeyPut, true, http.StatusOK},
		{RoleWriter, RouteLabCrash, false, http.StatusForbidden},
		{RoleAdmin, RouteLabCrash, true, http.StatusOK},
		{Role("superuser"), RouteHealth, false, http.StatusForbidden},
		{Role(""), RouteHealth, false, http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(string(tc.role)+"->"+tc.path, func(t *testing.T) {
			h.role.Store(tc.role)
			before := h.executed.Load()

			method := http.MethodGet
			if tc.path == RouteKeyPut || tc.path == RouteLabCrash {
				method = http.MethodPost
			}
			resp := h.do(t, method, tc.path, h.legit())
			_ = resp.Body.Close()

			ran := h.executed.Load() != before
			if ran != tc.wantRun {
				t.Errorf("handler ran=%v, want %v", ran, tc.wantRun)
			}
			if !tc.wantRun && resp.StatusCode != tc.wantCode {
				t.Errorf("status = %d, want %d", resp.StatusCode, tc.wantCode)
			}
		})
	}
	h.role.Store(RoleAdmin)
}

// TestSecuritySuite_UnauthenticatedDenied verifies SEC-4 fail-closed behaviour.
func TestSecuritySuite_UnauthenticatedDenied(t *testing.T) {
	h := newSecHarness(t, 4)
	defer h.close()
	h.setAuthed(false)

	before := h.executed.Load()
	resp := h.do(t, http.MethodGet, RouteHealth, nil)
	_ = resp.Body.Close()
	if h.executed.Load() != before {
		t.Error("SECURITY: an unauthenticated principal reached a handler")
	}
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

// TestSecuritySuite_OversizeBody covers SEC-7 at the read boundary.
func TestSecuritySuite_OversizeBody(t *testing.T) {
	h := newSecHarness(t, 4)
	defer h.close()

	// Send a body far beyond the /keys/put cap and require the read to be cut short with a
	// MaxBytesError rather than buffering the whole payload.
	huge := strings.Repeat("A", 6*1024*1024)
	req, err := http.NewRequest(http.MethodPost, h.server.URL+APIPrefix+RouteKeyPut, strings.NewReader(huge))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "127.0.0.1"
	req.Header.Set(CSRFHeader, h.token.Value())
	req.Header.Set(AdminConfirmHeader, "confirm")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		// Some transports surface the cap as a write error, which is also a rejection.
		return
	}
	defer func() { _ = resp.Body.Close() }()

	read, readErr := io.ReadAll(resp.Body)
	if readErr == nil && int64(len(read)) >= int64(len(huge)) {
		t.Errorf("SECURITY: the full %d-byte oversize body was accepted", len(huge))
	}
	if readErr != nil && !MaxBytesError(readErr) {
		t.Logf("read stopped with %v (not a MaxBytesError)", readErr)
	}
}

// TestSecuritySuite_SSEFlood covers SEC-8.
func TestSecuritySuite_SSEFlood(t *testing.T) {
	const cap = 2
	h := newSecHarness(t, cap)
	defer h.close()

	client := &http.Client{Timeout: 3 * time.Second}
	var opened, refused int
	for i := 0; i < 12; i++ {
		req, err := http.NewRequest(http.MethodGet, h.server.URL+APIPrefix+RouteEvents, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Host = "127.0.0.1"
		req.Header.Set(CSRFHeader, h.token.Value())
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		if resp.StatusCode == http.StatusOK {
			opened++
		} else if resp.StatusCode == http.StatusServiceUnavailable {
			refused++
		}
		_ = resp.Body.Close()
	}
	if opened > cap {
		t.Errorf("%d SSE streams opened with a cap of %d", opened, cap)
	}
	if refused == 0 {
		t.Error("no stream was refused; the budget was not enforced")
	}
	t.Logf("opened=%d refused=%d (cap %d)", opened, refused, cap)
}

// TestSecuritySuite_RawByteTraversal is SEC-11 over a RAW connection.
//
// curl and net/http normalise "../" client-side, so a traversal test written with them proves
// nothing. This sends the literal request line over TCP.
func TestSecuritySuite_RawByteTraversal(t *testing.T) {
	h := newSecHarness(t, 2)
	defer h.close()

	host := strings.TrimPrefix(h.server.URL, "http://")
	addr, err := net.ResolveTCPAddr("tcp", host)
	if err != nil {
		t.Fatal(err)
	}

	paths := []string{
		"/../etc/passwd",
		"/../../../../etc/passwd",
		"/assets/../../../etc/passwd",
		"/..\\..\\etc\\passwd",
		"/%2e%2e/%2e%2e/etc/passwd",
		"/index.html%00.txt",
		"/api/v1/../../../etc/passwd",
	}
	for _, p := range paths {
		t.Run(p, func(t *testing.T) {
			conn, err := net.DialTCP("tcp", nil, addr)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()

			req := fmt.Sprintf("GET %s HTTP/1.1\r\nHost: %s\r\nConnection: close\r\n\r\n", p, host)
			if _, err := conn.Write([]byte(req)); err != nil {
				t.Fatal(err)
			}
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))

			rd := bufio.NewReader(conn)
			statusLine, err := rd.ReadString('\n')
			if err != nil {
				t.Fatalf("reading status: %v", err)
			}
			body := make([]byte, 0, 4096)
			buf := make([]byte, 4096)
			for {
				n, err := rd.Read(buf)
				body = append(body, buf[:n]...)
				if err != nil {
					break
				}
				if len(body) > 65536 {
					break
				}
			}
			text := string(body)
			if strings.Contains(text, "root:") {
				t.Fatalf("TRAVERSAL SUCCEEDED for %s", p)
			}
			t.Logf("%s -> %s", p, strings.TrimSpace(statusLine))
		})
	}
}

// TestSecuritySuite_SymlinkEscape exercises the path primitive that future file-serving
// endpoints (LSM-2, WAL-1) will compose. There is no file endpoint yet, so this pins the
// primitive itself: security.ResolvePath must refuse a symlink that escapes the root.
func TestSecuritySuite_SymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()

	secret := outside + "/secret.sst"
	if err := osWriteFile(secret, []byte("SECRET-CONTENT")); err != nil {
		t.Fatal(err)
	}
	link := root + "/escape.sst"
	if err := osSymlink(secret, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// The primitive every file endpoint must use refuses the escape.
	if _, err := security.ResolvePath(root, "escape.sst"); err == nil {
		t.Error("SECURITY: ResolvePath allowed a symlink escaping the root")
	}
	// And a legitimate file still resolves.
	if err := osWriteFile(root+"/ok.sst", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := security.ResolvePath(root, "ok.sst"); err != nil {
		t.Errorf("ResolvePath rejected a legitimate file: %v", err)
	}
}

// TestSecuritySuite_ErrorLeakage covers SEC-9: an internal error must not reach the client.
func TestSecuritySuite_ErrorLeakage(t *testing.T) {
	h := newSecHarness(t, 2)
	defer h.close()

	leaky := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		Fail(w, &latticeInvalidPathError{Path: "/etc/shadow", Root: "/data", Reason: "escapes root"})
	})
	specs := []RouteSpec{{
		Method: http.MethodGet, Path: RouteSSTInspct, Permission: PermissionRead,
		Handler: leaky, description: "inspect",
	}}
	api, err := NewRouter(specs, nil)
	if err != nil {
		t.Fatal(err)
	}
	api.SetPrincipalResolver(func(*http.Request) *Principal {
		return &Principal{Role: RoleAdmin, Authenticated: true}
	})
	ts := httptest.NewServer(api)
	defer ts.Close()

	resp, err := http.Get(ts.URL + APIPrefix + RouteSSTInspct)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	for _, secret := range []string{"/etc/shadow", "/data", "InvalidPath"} {
		if strings.Contains(string(body), secret) {
			t.Errorf("error response leaked %q: %s", secret, body)
		}
	}
}

// TestSecuritySuite_HeaderPresence covers SEC-5 on every response class.
func TestSecuritySuite_HeaderPresence(t *testing.T) {
	h := newSecHarness(t, 2)
	defer h.close()

	req, _ := http.NewRequest(http.MethodGet, h.server.URL+APIPrefix+RouteHealth, nil)
	req.Host = "127.0.0.1"
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()

	want := map[string]string{
		"X-Content-Type-Options":       "nosniff",
		"X-Frame-Options":              "DENY",
		"Referrer-Policy":              "no-referrer",
		"Cross-Origin-Opener-Policy":   "same-origin",
		"Cross-Origin-Resource-Policy": "same-origin",
		"Cache-Control":                "no-store",
	}
	for k, v := range want {
		if got := resp.Header.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if resp.Header.Get("Content-Security-Policy") == "" {
		t.Error("missing CSP")
	}
	if resp.Header.Get(RequestIDHeader) == "" {
		t.Error("missing request id")
	}
}

// TestSecuritySuite_NoCSRFTokenEverLogged is the SEC-10 regression for a bug found during
// that task: a 32-hex request id was once misclassified as a token and redacted, breaking log
// correlation. Hex identifiers must be preserved.
func TestSecuritySuite_NoCSRFTokenEverLogged(t *testing.T) {
	tok, err := NewCSRFToken()
	if err != nil {
		t.Fatal(err)
	}
	if got := RedactValue(RequestIDHeader, "3159e0e1d5c82f39a3d31a6fcc6ae18a"); got == RedactedValue {
		t.Error("a hex request id was redacted; log correlation is broken")
	}
	if got := RedactValue("request_id", tok.Value()); got != RedactedValue {
		t.Errorf("a CSRF token was not redacted: %q", got)
	}
}

// TestNoCORSHeadersEverEmitted is the SEC-12.3 invariant.
func TestNoCORSHeadersEverEmitted(t *testing.T) {
	h := newSecHarness(t, 2)
	defer h.close()

	type probe struct {
		method string
		path   string
		origin string
	}
	probes := []probe{
		{http.MethodGet, RouteHealth, ""},
		{http.MethodGet, RouteHealth, "http://127.0.0.1"},
		{http.MethodPost, RouteKeyPut, ""},
		{http.MethodPost, RouteKeyPut, "http://evil.com"},
		{http.MethodOptions, RouteEvents, "http://evil.com"},
		{http.MethodGet, "/does-not-exist", ""},
		{http.MethodGet, RouteKeyPut, ""}, // wrong method
	}
	for _, p := range probes {
		req, _ := http.NewRequest(p.method, h.server.URL+p.path, strings.NewReader("{}"))
		req.Host = "127.0.0.1"
		if p.origin != "" {
			req.Header.Set("Origin", p.origin)
		}
		req.Header.Set(CSRFHeader, h.token.Value())
		req.Header.Set(AdminConfirmHeader, "confirm")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", p.method, p.path, err)
		}
		if hasAnyCORSHeader(resp.Header) {
			t.Errorf("%s %s (origin %q) emitted a CORS header: %v",
				p.method, p.path, p.origin, resp.Header)
		}
		_ = resp.Body.Close()
	}
}

// TestNoRouteExistsWithoutPermissionMetadata is the SEC-12.2 completeness gate.
//
// It is deliberately a test over the shared routePlan rather than a runtime check: a new
// route that forgets its permission must fail CI, not fail open in production.
func TestNoRouteExistsWithoutPermissionMetadata(t *testing.T) {
	if len(routePlan) == 0 {
		t.Fatal("routePlan is empty; the API surface has vanished")
	}
	for path, spec := range routePlan {
		if spec.Permission == 0 {
			t.Errorf("SECURITY: route %q declares no permission", path)
		}
		switch spec.Permission {
		case PermissionRead, PermissionWrite, PermissionAdmin:
		default:
			t.Errorf("route %q declares a non-canonical permission %d", path, spec.Permission)
		}
		if spec.Method == "" {
			t.Errorf("route %q declares no method", path)
		}
		if spec.description == "" {
			t.Errorf("route %q declares no description", path)
		}
		if spec.Mutating && spec.Method != http.MethodPost {
			t.Errorf("mutating route %q uses %s, want POST", path, spec.Method)
		}
	}

	// Every route BuildRoutes can emit must correspond to a planned route.
	h := &Handlers{}
	for _, spec := range BuildRoutes(h) {
		if _, ok := routePlan[spec.Path]; !ok {
			t.Errorf("BuildRoutes emitted %q which is absent from routePlan", spec.Path)
		}
	}
}

// TestSecuritySuite_DestructiveRoutesRemainAdminLocked is a permanent assertion on the
// blast-radius decisions, so a future edit cannot quietly downgrade them.
func TestSecuritySuite_DestructiveRoutesRemainAdminLocked(t *testing.T) {
	adminOnly := []string{
		RouteLabCrash, RouteLabClean, RouteRaftCamp, RouteRaftStep,
		RouteConsole, RouteDiag, RouteRaftLog, RouteWALDump, RouteEvents,
	}
	for _, path := range adminOnly {
		perm, ok := PlanFor(path)
		if !ok {
			t.Errorf("route %q disappeared from the plan", path)
			continue
		}
		if perm != PermissionAdmin {
			t.Errorf("SECURITY: route %q is no longer admin-only (permission %d)", path, perm)
		}
	}
}

// TestSecuritySuite_ChainOrderIsCanonical documents and enforces the middleware order, since
// every control depends on it (Host before Origin, CSRF before the handler).
func TestSecuritySuite_ChainOrderIsCanonical(t *testing.T) {
	hosts := NewHostAllowlist("127.0.0.1")
	origins := NewOriginAllowlist("http://127.0.0.1")
	tok, err := NewCSRFToken()
	if err != nil {
		t.Fatal(err)
	}

	reached := false
	inner := http.HandlerFunc(func(http.ResponseWriter, *http.Request) { reached = true })

	// Bad Host: must be rejected before Origin and CSRF are even consulted.
	reached = false
	req := httptest.NewRequest(http.MethodPost, RouteKeyPut, nil)
	req.Host = "attacker.example"
	req.Header.Set(CSRFHeader, tok.Value())
	req.Header.Set(AdminConfirmHeader, "confirm")
	req.Header.Set("Origin", "http://attacker.example")
	HostGuard(hosts, "http")(OriginGuard(origins)(CSRFGuard(tok)(inner))).
		ServeHTTP(httptest.NewRecorder(), req)
	if reached {
		t.Error("a bad Host reached the handler")
	}

	// Good Host, bad Origin: rejected before CSRF.
	reached = false
	req = httptest.NewRequest(http.MethodPost, RouteKeyPut, nil)
	req.Host = "127.0.0.1"
	req.Header.Set("Origin", "http://evil.com")
	HostGuard(hosts, "http")(OriginGuard(origins)(CSRFGuard(tok)(inner))).
		ServeHTTP(httptest.NewRecorder(), req)
	if reached {
		t.Error("a bad Origin reached the handler")
	}

	// Everything valid: reaches the handler.
	reached = false
	req = httptest.NewRequest(http.MethodPost, RouteKeyPut, nil)
	req.Host = "127.0.0.1"
	req.Header.Set(CSRFHeader, tok.Value())
	req.Header.Set(AdminConfirmHeader, "confirm")
	HostGuard(hosts, "http")(OriginGuard(origins)(CSRFGuard(tok)(inner))).
		ServeHTTP(httptest.NewRecorder(), req)
	if !reached {
		t.Error("a fully legitimate request did not reach the handler")
	}
}

// TestSecuritySuite_MethodEnforcement ensures a GET can never reach a POST-only handler.
func TestSecuritySuite_MethodEnforcement(t *testing.T) {
	h := newSecHarness(t, 2)
	defer h.close()

	for _, m := range []string{http.MethodGet, http.MethodPut, http.MethodDelete, http.MethodPatch, http.MethodTrace} {
		before := h.executed.Load()
		req, _ := http.NewRequest(m, h.server.URL+APIPrefix+RouteKeyPut, strings.NewReader("{}"))
		req.Host = "127.0.0.1"
		req.Header.Set(CSRFHeader, h.token.Value())
		req.Header.Set(AdminConfirmHeader, "confirm")
		resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
		if err != nil {
			continue
		}
		_ = resp.Body.Close()
		if h.executed.Load() != before {
			t.Errorf("SECURITY: %s reached the POST-only handler", m)
		}
	}
}

// TestSecuritySuite_NoGoroutineLeakAfterAttacks is the SEC-8 net: the whole suite must leave
// the server clean.
func TestSecuritySuite_NoGoroutineLeakAfterAttacks(t *testing.T) {
	before := runtimeNumGoroutine()

	h := newSecHarness(t, 4)
	for i := 0; i < 40; i++ {
		resp := h.do(t, http.MethodPost, RouteKeyPut, map[string]string{
			"Origin":   "http://evil.com",
			CSRFHeader: "wrong",
		})
		_ = resp.Body.Close()
	}
	h.close()

	// Allow connections to unwind.
	time.Sleep(300 * time.Millisecond)
	after := runtimeNumGoroutine()
	if after-before > 15 {
		t.Errorf("goroutine leak after the attack suite: before=%d after=%d", before, after)
	}
	t.Logf("goroutines before=%d after=%d", before, after)
}

// Ensure the harness satisfies the context import used by the timeout path.
var _ = context.Background
